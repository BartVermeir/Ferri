package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"sync"
	"time"
)

// Upload diagnostics (storage.diag_upload_hashes in config.yaml). When on,
// every upload is hashed per block while it is received, and once it is
// finished the stored file is read back and hashed the same way. A block
// whose two hashes differ changed between receiving and storing it. The
// received hashes, compared with the original file (same block size, same
// offsets), show whether the client sent the right bytes. Memory only: a
// restart forgets everything.

// DiagBlockSize divides the upload chunk size (200 MiB, upload.js), so a
// chunk that starts on a chunk boundary also starts on a block boundary.
const DiagBlockSize = 8 << 20

const (
	diagMaxUploads    = 50  // older uploads are forgotten
	diagVerifyQueue   = 100 // finished uploads waiting to be read back; more are skipped
	diagReadParallel  = 4   // blocks read back at once
	diagStateVerified = "verified"
)

type diagSum struct {
	sum [sha256.Size]byte
	ok  bool
}

func (s diagSum) String() string {
	if !s.ok {
		return "-"
	}
	return hex.EncodeToString(s.sum[:])
}

type diagUpload struct {
	id, name   string
	size       int64
	started    time.Time
	state      string
	received   []diagSum
	stored     []diagSum
	storedSize int64
	mismatches int

	// The block being hashed between chunks: next is the offset the next
	// chunk must start at to continue it. nil = start over at the next
	// block boundary.
	next  int64
	block hash.Hash
}

type diagRecorder struct {
	blockSize int64
	open      func(path string) (io.ReadSeekCloser, error)
	verify    chan string

	mu      sync.Mutex
	uploads map[string]*diagUpload
	order   []string // oldest first
}

func newDiagRecorder(blockSize int64, open func(string) (io.ReadSeekCloser, error)) *diagRecorder {
	return &diagRecorder{
		blockSize: blockSize,
		open:      open,
		verify:    make(chan string, diagVerifyQueue),
		uploads:   map[string]*diagUpload{},
	}
}

func (d *diagRecorder) run() {
	for id := range d.verify {
		d.verifyOne(id)
	}
}

// get returns the upload's record, creating it; d.mu must be held.
func (d *diagRecorder) get(id, name string, size int64) *diagUpload {
	u := d.uploads[id]
	if u == nil {
		u = &diagUpload{id: id, started: time.Now(), state: "receiving", next: -1}
		d.uploads[id] = u
		d.order = append(d.order, id)
		for len(d.order) > diagMaxUploads {
			delete(d.uploads, d.order[0])
			d.order = d.order[1:]
		}
	}
	if name != "" {
		u.name = name
	}
	if size > 0 {
		u.size = size
	}
	return u
}

// begin wraps one chunk's body. The caller passes the bytes the store wrote
// to done.
func (d *diagRecorder) begin(id, name string, size, offset int64, src io.Reader) *diagReader {
	d.mu.Lock()
	defer d.mu.Unlock()
	u := d.get(id, name, size)
	r := &diagReader{d: d, u: u, src: src, start: offset, pos: offset}
	switch {
	case u.block != nil && u.next == offset:
		r.h = u.block
	case offset%d.blockSize == 0:
		r.h = sha256.New()
	}
	// Taken over by this chunk: a parallel chunk of the same upload starts
	// its own block instead of writing into this one.
	u.block, u.next = nil, -1
	return r
}

// diagReader hashes a chunk's bytes as the store reads them. Its state is
// only touched by the store's reading goroutine until done.
type diagReader struct {
	d       *diagRecorder
	u       *diagUpload
	src     io.Reader
	start   int64
	pos     int64
	h       hash.Hash // nil while skipping to the next block boundary
	pending []pendingSum
}

type pendingSum struct {
	index int64
	sum   diagSum
}

func (r *diagReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	b := p[:n]
	bs := r.d.blockSize
	for len(b) > 0 {
		end := (r.pos/bs + 1) * bs
		take := int64(len(b))
		if take > end-r.pos {
			take = end - r.pos
		}
		if r.h != nil {
			r.h.Write(b[:take])
		}
		r.pos += take
		b = b[take:]
		if r.pos == end {
			if r.h != nil {
				s := diagSum{ok: true}
				r.h.Sum(s.sum[:0])
				r.pending = append(r.pending, pendingSum{index: end/bs - 1, sum: s})
			}
			r.h = sha256.New()
		}
	}
	return n, err
}

// done records the blocks the store wrote. A block read but not written is
// sent again by the client, maybe with other bytes, so it is not kept.
func (r *diagReader) done(written int64, err error) {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	u := r.u
	end := r.start + written
	for _, p := range r.pending {
		if (p.index+1)*r.d.blockSize > end {
			continue
		}
		for int64(len(u.received)) <= p.index {
			u.received = append(u.received, diagSum{})
		}
		u.received[p.index] = p.sum
	}
	if err == nil && r.pos == end {
		u.block, u.next = r.h, end
	}
}

// finish closes the last, short block and queues the read-back.
func (d *diagRecorder) finish(id, name string, size int64) {
	d.mu.Lock()
	u := d.get(id, name, size)
	u.size = size
	if u.block != nil && u.next == size && size%d.blockSize != 0 {
		i := size / d.blockSize
		for int64(len(u.received)) <= i {
			u.received = append(u.received, diagSum{})
		}
		s := diagSum{ok: true}
		u.block.Sum(s.sum[:0])
		u.received[i] = s
	}
	u.block, u.next = nil, -1
	u.state = "queued"
	d.mu.Unlock()

	select {
	case d.verify <- id:
	default:
		d.setState(id, "skipped: read-back queue full")
		slog.Warn("diag: read-back queue full, upload not verified", "tus_id", id)
	}
}

func (d *diagRecorder) setState(id, state string) {
	d.mu.Lock()
	if u := d.uploads[id]; u != nil {
		u.state = state
	}
	d.mu.Unlock()
}

// verifyOne reads the stored file back, hashes it per block and compares
// every block with what was received.
func (d *diagRecorder) verifyOne(id string) {
	d.mu.Lock()
	u := d.uploads[id]
	if u == nil {
		d.mu.Unlock()
		return
	}
	u.state = "verifying"
	size := u.size
	d.mu.Unlock()

	stored, storedSize, err := d.readBack(id, size)
	if err != nil {
		d.setState(id, "error: "+err.Error())
		slog.Warn("diag: read back failed", "tus_id", id, "error", err)
		return
	}

	d.mu.Lock()
	u.stored, u.storedSize = stored, storedSize
	u.mismatches = 0
	unknown := 0
	type diff struct {
		index            int
		received, stored string
	}
	var diffs []diff
	for i, s := range stored {
		var r diagSum
		if i < len(u.received) {
			r = u.received[i]
		}
		switch {
		case !r.ok:
			unknown++
		case !s.ok || r.sum != s.sum:
			u.mismatches++
			diffs = append(diffs, diff{i, r.String(), s.String()})
		}
	}
	u.state = diagStateVerified
	mismatches := u.mismatches
	d.mu.Unlock()

	if storedSize != size {
		slog.Warn("diag: stored size differs", "tus_id", id, "size", size, "stored_size", storedSize)
	}
	for _, df := range diffs {
		slog.Warn("diag: stored block differs from received", "tus_id", id, "block", df.index,
			"offset", int64(df.index)*d.blockSize, "received", df.received, "stored", df.stored)
	}
	log := slog.Info
	if mismatches > 0 || storedSize != size {
		log = slog.Warn
	}
	log("diag: upload verified", "tus_id", id, "size", size, "blocks", len(stored),
		"not_measured", unknown, "differ", mismatches)
}

// readBack hashes the stored file per block, up to the upload's size; a
// block missing on storage stays unknown.
func (d *diagRecorder) readBack(id string, size int64) ([]diagSum, int64, error) {
	f, err := d.open(id)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	ra, ok := f.(io.ReaderAt)
	if !ok {
		return nil, 0, errors.New("stored file cannot be read at an offset")
	}
	storedSize, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, 0, fmt.Errorf("seek: %w", err)
	}

	bs := d.blockSize
	blocks := (size + bs - 1) / bs
	sums := make([]diagSum, blocks)
	next := make(chan int64)
	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		firstErr error
	)
	for w := 0; w < diagReadParallel; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, bs)
			for i := range next {
				off := i * bs
				want := min(bs, size-off)
				n, err := ra.ReadAt(buf[:want], off)
				if err != nil && !errors.Is(err, io.EOF) {
					errMu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("read at %d: %w", off, err)
					}
					errMu.Unlock()
					continue
				}
				if int64(n) < want {
					continue // stored file too short: this block stays unknown
				}
				sums[i] = diagSum{ok: true, sum: sha256.Sum256(buf[:n])}
			}
		}()
	}
	for i := int64(0); i < blocks; i++ {
		next <- i
	}
	close(next)
	wg.Wait()
	return sums, storedSize, firstErr
}

// DiagUpload is one measured upload, as listed on /admin/diag.
type DiagUpload struct {
	ID, Name, State string
	Size            int64
	StoredSize      int64
	Started         time.Time
	Blocks          int64
	NotMeasured     int // blocks without a received hash (resumed mid-block)
	Differ          int
}

// DiagEnabled reports whether upload diagnostics are on.
func (m *Manager) DiagEnabled() bool { return m.diag != nil }

// EnableUploadDiag turns upload diagnostics on. Call once at startup, before
// the first upload.
func (m *Manager) EnableUploadDiag() {
	m.diag = newDiagRecorder(DiagBlockSize, m.Open)
	go m.diag.run()
}

// DiagUploads lists the measured uploads, newest first.
func (m *Manager) DiagUploads() []DiagUpload {
	d := m.diag
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]DiagUpload, 0, len(d.order))
	for i := len(d.order) - 1; i >= 0; i-- {
		u := d.uploads[d.order[i]]
		blocks := (u.size + d.blockSize - 1) / d.blockSize
		notMeasured := 0
		for b := int64(0); b < blocks; b++ {
			if b >= int64(len(u.received)) || !u.received[b].ok {
				notMeasured++
			}
		}
		out = append(out, DiagUpload{
			ID: u.id, Name: u.name, State: u.state, Size: u.size, StoredSize: u.storedSize,
			Started: u.started, Blocks: blocks, NotMeasured: notMeasured, Differ: u.mismatches,
		})
	}
	return out
}

// WriteDiagBlocks writes one line per block: "<offset> <received> <stored>",
// "-" where a hash is unknown. false when the upload is not known.
func (m *Manager) WriteDiagBlocks(id string, w io.Writer) bool {
	d := m.diag
	if d == nil {
		return false
	}
	d.mu.Lock()
	u := d.uploads[id]
	if u == nil {
		d.mu.Unlock()
		return false
	}
	blocks := max(int64(len(u.received)), int64(len(u.stored)), (u.size+d.blockSize-1)/d.blockSize)
	lines := make([]string, 0, blocks)
	for i := int64(0); i < blocks; i++ {
		var r, s diagSum
		if i < int64(len(u.received)) {
			r = u.received[i]
		}
		if i < int64(len(u.stored)) {
			s = u.stored[i]
		}
		lines = append(lines, fmt.Sprintf("%d %s %s\n", i*d.blockSize, r, s))
	}
	d.mu.Unlock()
	for _, l := range lines {
		if _, err := io.WriteString(w, l); err != nil {
			break
		}
	}
	return true
}
