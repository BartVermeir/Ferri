package handler

import (
	"database/sql"
	"errors"
	"io"
	"sync"

	"github.com/BartVermeir/Ferri/internal/storage"
)

// openStored opens an uploaded file's data. The bytes live at the flat TUS
// path (tus_upload_id); storage_path is only a real file for legacy local
// uploads, so it is the fallback.
func openStored(mgr *storage.Manager, storagePath string, tusUploadID sql.NullString) (io.ReadSeekCloser, error) {
	if tusUploadID.Valid && tusUploadID.String != "" {
		if f, err := mgr.Open(tusUploadID.String); err == nil {
			return f, nil
		}
	}
	return mgr.Open(storagePath)
}

// fileReadAheadSize is how much a download reads from storage per request.
// http.ServeContent copies in 32KB steps; on SMB every read is a round trip
// to the share. 1MB is go-smb2's ceiling per SMB read.
const fileReadAheadSize = 1 << 20 // 1MB

// fileReadAheadDepth is how many blocks are requested from storage at once.
// go-smb2 waits for each read's answer before sending the next. On SMB the
// blocks are spread over the backend's connections (storage.stripedFile).
// Memory: depth+1 blocks of 1MB per running download.
const fileReadAheadDepth = 8

// block is one fileReadAheadSize read from storage, running or done.
type block struct {
	off  int64
	buf  []byte
	n    int
	err  error
	done chan struct{}
}

// readAhead serves small reads from large blocks read from src. Seeks are
// cheap: a seek inside the current block keeps it (ServeContent sniffs the
// first 512 bytes and seeks back to 0), any other seek only takes effect on
// the next read.
//
// When src is an io.ReaderAt (local files and SMB files both are), up to
// fileReadAheadDepth blocks ahead of the reader are read in parallel. Call
// Close before closing src: it waits for reads still running.
type readAhead struct {
	src io.ReadSeeker
	at  io.ReaderAt // nil: one block at a time through src
	pos int64       // logical offset

	// One block at a time (src is not an io.ReaderAt).
	buf    []byte
	start  int64 // file offset of buf[0]
	n      int   // valid bytes in buf
	srcPos int64 // offset of src

	// Parallel (src is an io.ReaderAt).
	cur   *block   // block being served
	queue []*block // requested blocks at consecutive offsets after cur
	next  int64    // offset of the next block to request
	size  int64    // file size, -1 until a SeekEnd or a short block tells
	free  [][]byte
	wg    sync.WaitGroup
}

func newReadAhead(src io.ReadSeeker) *readAhead {
	r := &readAhead{src: src, size: -1}
	if at, ok := src.(io.ReaderAt); ok {
		r.at = at
	} else {
		r.buf = make([]byte, fileReadAheadSize)
	}
	return r
}

func (r *readAhead) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.at != nil {
		return r.readParallel(p)
	}
	if r.pos < r.start || r.pos >= r.start+int64(r.n) {
		if r.srcPos != r.pos {
			if _, err := r.src.Seek(r.pos, io.SeekStart); err != nil {
				return 0, err
			}
			r.srcPos = r.pos
		}
		m, err := io.ReadFull(r.src, r.buf)
		r.srcPos += int64(m)
		r.start, r.n = r.pos, m
		if m == 0 {
			return 0, endErr(err)
		}
		// A short fill (end of file, or an error) still hands out what it
		// got; the next refill reports the EOF or the error.
	}
	c := copy(p, r.buf[r.pos-r.start:r.n])
	r.pos += int64(c)
	return c, nil
}

func (r *readAhead) readParallel(p []byte) (int, error) {
	if b := r.cur; b == nil || r.pos < b.off || r.pos >= b.off+int64(b.n) {
		// A short block ends the data: past it is its EOF or its error.
		if b != nil && b.n < len(b.buf) && r.pos == b.off+int64(b.n) {
			return 0, endErr(b.err)
		}
		if len(r.queue) == 0 || r.queue[0].off != r.pos {
			r.restartAt(r.pos)
		}
		r.fill()
		if len(r.queue) == 0 {
			return 0, io.EOF // at or past the known end of the file
		}
		nb := r.queue[0]
		r.queue = r.queue[1:]
		<-nb.done
		if r.cur != nil {
			r.free = append(r.free, r.cur.buf)
		}
		r.cur = nb
		if nb.n < len(nb.buf) {
			// End of file (or an error) here: request nothing past it.
			if end := nb.off + int64(nb.n); r.size < 0 || end < r.size {
				r.size = end
			}
		}
		r.fill()
		if nb.n == 0 {
			return 0, endErr(nb.err)
		}
	}
	b := r.cur
	c := copy(p, b.buf[r.pos-b.off:b.n])
	r.pos += int64(c)
	return c, nil
}

// fill requests blocks until fileReadAheadDepth are in flight or the end of
// the file is reached.
func (r *readAhead) fill() {
	for len(r.queue) < fileReadAheadDepth && (r.size < 0 || r.next < r.size) {
		var buf []byte
		if n := len(r.free); n > 0 {
			buf, r.free = r.free[n-1], r.free[:n-1]
		} else {
			buf = make([]byte, fileReadAheadSize)
		}
		b := &block{off: r.next, buf: buf, done: make(chan struct{})}
		r.next += int64(len(buf))
		r.queue = append(r.queue, b)
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			b.n, b.err = r.at.ReadAt(b.buf, b.off)
			close(b.done)
		}()
	}
}

// restartAt drops the requested blocks (after a seek) and continues at off.
// It waits for them: their buffers go back to the free list.
func (r *readAhead) restartAt(off int64) {
	for _, b := range r.queue {
		<-b.done
		r.free = append(r.free, b.buf)
	}
	r.queue = nil
	r.next = off
}

// Close waits for reads from storage that are still running, so src can be
// closed safely after it. The readAhead cannot be used afterwards.
func (r *readAhead) Close() error {
	r.wg.Wait()
	return nil
}

func (r *readAhead) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.pos + offset
	case io.SeekEnd:
		end, err := r.src.Seek(0, io.SeekEnd)
		if err != nil {
			return 0, err
		}
		r.srcPos = end
		if r.at != nil {
			r.size = end
		}
		abs = end + offset
	default:
		return 0, errors.New("readAhead: invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("readAhead: negative position")
	}
	r.pos = abs
	return abs, nil
}

// endErr turns what a read returned at the end of the data into the error
// Read reports: io.EOF for the end of the file, the error itself otherwise.
func endErr(err error) error {
	if err == nil || errors.Is(err, io.ErrUnexpectedEOF) {
		return io.EOF
	}
	return err
}
