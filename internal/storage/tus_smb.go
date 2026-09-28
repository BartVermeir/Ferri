package storage

// SMB-backed tusd DataStore.
//
// TUS files are stored flat on the share:
//   <basePath>/<upload_id>       — binary upload data
//   <basePath>/<upload_id>.info  — JSON metadata (FileInfo)
//
// This mirrors tusd's own filestore layout so the rest of Ferri's code
// (which references tus_upload_id) works identically for both backends.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
	smb2 "github.com/hirochachacha/go-smb2"
	tusd "github.com/tus/tusd/v2/pkg/handler"
)

// smbTUSStore is a tusd.DataStore backed by an SMB share. Every share
// operation goes through the backend's withShare, which reconnects once when
// the connection or session is gone, so an idle session the server dropped
// does not break uploads.
type smbTUSStore struct {
	b        *SMBBackend
	basePath string // path prefix on the share, e.g. "" or "ferri"
}

func newSMBTUSStore(b *SMBBackend, basePath string) *smbTUSStore {
	return &smbTUSStore{b: b, basePath: basePath}
}

// smbPath returns the full path on the share for a given name.
func (s *smbTUSStore) smbPath(name string) string {
	if s.basePath == "" || s.basePath == "/" {
		return name
	}
	return s.basePath + "/" + name
}

// NewUpload creates a new TUS upload on the SMB share.
func (s *smbTUSStore) NewUpload(ctx context.Context, info tusd.FileInfo) (tusd.Upload, error) {
	if info.ID == "" {
		info.ID = uuid.New().String()
	}

	// Create empty data file
	binPath := s.smbPath(info.ID)
	err := s.b.withShare(func(sh *smb2.Share) error {
		f, err := sh.OpenFile(binPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		return f.Close()
	})
	if err != nil {
		return nil, fmt.Errorf("smb tus: create data file %q: %w", binPath, err)
	}

	// Write .info file. A new upload keeps its offset in a .offset file (see
	// resumeOffset); the .info file says so. Created after the .info file: a
	// marked upload without a .offset file resumes at 0, which is safe.
	if info.Storage == nil {
		info.Storage = map[string]string{}
	}
	info.Storage[storageKeyTracked] = "1"
	upload := &smbUpload{store: s, info: info}
	if err := upload.writeInfo(); err != nil {
		return nil, fmt.Errorf("smb tus: write info file: %w", err)
	}
	if err := upload.writeOffset(0); err != nil {
		return nil, fmt.Errorf("smb tus: write offset file: %w", err)
	}

	return upload, nil
}

// GetUpload retrieves an existing TUS upload from the SMB share. Only a
// missing .info file is ErrNotFound; any other error is returned as is, so
// after a network hiccup the browser retries the upload instead of dropping it.
func (s *smbTUSStore) GetUpload(ctx context.Context, id string) (tusd.Upload, error) {
	infoPath := s.smbPath(id + ".info")
	binPath := s.smbPath(id)
	var data []byte
	var size int64 = -1
	err := s.b.withShare(func(sh *smb2.Share) error {
		f, err := sh.Open(infoPath)
		if err != nil {
			return err
		}
		defer f.Close()
		if data, err = io.ReadAll(f); err != nil {
			return err
		}
		if fi, err := sh.Stat(binPath); err == nil {
			size = fi.Size()
		}
		return nil
	})
	if isNotExist(err) {
		return nil, tusd.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("smb tus: read info file %q: %w", infoPath, err)
	}

	var info tusd.FileInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("smb tus: parse info file %q: %w", infoPath, err)
	}

	tracked := info.Storage[storageKeyTracked] == "1"
	var recorded int64
	if tracked {
		var err error
		if recorded, err = s.readOffset(id); err != nil {
			return nil, err
		}
	}
	info.Offset = resumeOffset(tracked, recorded, size, info.Offset)

	return &smbUpload{store: s, info: info}, nil
}

// ── smbUpload ─────────────────────────────────────────────────────────────────

type smbUpload struct {
	store *smbTUSStore
	info  tusd.FileInfo
}

func (u *smbUpload) GetInfo(ctx context.Context) (tusd.FileInfo, error) {
	return u.info, nil
}

// writeBufSize matches the SMB2/3 payload ceiling (see go-smb2's
// winMaxPayloadSize): one SMB WRITE per block. Equal to stripeBlock, so each
// block goes over one connection.
const writeBufSize = stripeBlock

// writeDepth is how many blocks are written to the share at once. go-smb2
// waits for each write's answer before the next one, so one block at a time
// capped uploads far below what a fast LAN carries. The share also limits
// each connection (~2000 Mbps written; one connection reached that with 4
// blocks in flight, 8 added nothing), so the blocks are striped over
// smbConnections connections, 4 in flight on each. Memory: writeDepth blocks
// of 1MB per running upload chunk.
const writeDepth = 4 * smbConnections

// storageKeyTracked in the .info file marks an upload whose offset is kept in
// <id>.offset, updated after every chunk. Its blocks are written in parallel,
// so after a failed write the data file can be longer than what is complete:
// its size is no longer a safe offset to resume from. Uploads created before
// this lack the mark; they are written one block at a time, so for them the
// size still is.
const storageKeyTracked = "ferri_offset_file"

// resumeOffset is where an upload continues. A tracked upload resumes at its
// recorded offset, never past the end of the data file; an untracked one at
// the data file's size (or, if that is unknown, the .info offset).
func resumeOffset(tracked bool, recorded, size, infoOffset int64) int64 {
	if tracked {
		if size >= 0 && size < recorded {
			return size
		}
		return recorded
	}
	if size >= 0 {
		return size
	}
	return infoOffset
}

// offsetPath is the file holding a tracked upload's offset.
func (s *smbTUSStore) offsetPath(id string) string {
	return s.smbPath(id + ".offset")
}

// readOffset reads a tracked upload's offset. A missing or unreadable file
// means 0: resending from the start is always safe, resuming past a gap is
// not. Other errors (the share is gone) are returned, so tusd retries.
func (s *smbTUSStore) readOffset(id string) (int64, error) {
	var data []byte
	err := s.b.withShare(func(sh *smb2.Share) error {
		f, err := sh.Open(s.offsetPath(id))
		if err != nil {
			return err
		}
		defer f.Close()
		data, err = io.ReadAll(f)
		return err
	})
	if isNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("smb tus: read offset file for %q: %w", id, err)
	}
	off, perr := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if perr != nil || off < 0 {
		slog.Warn("smb tus: unreadable offset file, resuming from 0", "id", id, "content", string(data))
		return 0, nil
	}
	return off, nil
}

// writeOffset records a tracked upload's offset. Fixed width, written over
// the old value at position 0 and never truncated: the file is never empty or
// half old, half new. Rewriting the same value is harmless, so a retry after
// a reconnect is safe.
func (u *smbUpload) writeOffset(off int64) error {
	p := u.store.offsetPath(u.info.ID)
	return u.store.b.withShare(func(sh *smb2.Share) error {
		f, err := sh.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		if _, err := f.WriteAt([]byte(fmt.Sprintf("%020d", off)), 0); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	})
}

// WriteChunk writes src to the data file starting at offset and, for a
// tracked upload, records the new offset. Only opening the file may reconnect
// and retry: once bytes of src are consumed, a retry cannot get them back. A
// failure returns the bytes written without a gap; the client then asks for
// the offset (GetUpload) and resends from there, overwriting whatever a later
// block may have written.
func (u *smbUpload) WriteChunk(ctx context.Context, offset int64, src io.Reader) (int64, error) {
	binPath := u.store.smbPath(u.info.ID)
	// Not O_APPEND: go-smb2 then opens with append-only access, and the
	// blocks are written each at its own offset.
	open := func(sh *smb2.Share) (*smb2.File, error) { return sh.OpenFile(binPath, os.O_WRONLY, 0o644) }

	// A tracked upload is written in parallel over all connections
	// (stripedFile); an untracked one, one block at a time on one.
	tracked := u.info.Storage[storageKeyTracked] == "1"
	var f io.Closer
	var w io.WriterAt
	depth := 1
	if tracked {
		sf, err := u.store.b.openStriped(open)
		if err != nil {
			return 0, fmt.Errorf("smb tus: open data file for write %q: %w", binPath, err)
		}
		f, w, depth = sf, sf, writeDepth
	} else {
		var one *smb2.File
		err := u.store.b.withShare(func(sh *smb2.Share) error {
			var err error
			one, err = open(sh)
			return err
		})
		if err != nil {
			return 0, fmt.Errorf("smb tus: open data file for write %q: %w", binPath, err)
		}
		f, w = one, one
	}
	defer f.Close()

	n, err := parallelWrite(w, offset, src, writeBufSize, depth)
	u.info.Offset = offset + n
	if tracked && n > 0 {
		// Not an error for the chunk: the data is written. With the old
		// offset on record the client resends from there, which is safe.
		if werr := u.writeOffset(u.info.Offset); werr != nil {
			slog.Warn("smb tus: record upload offset", "id", u.info.ID, "offset", u.info.Offset, "error", werr)
		}
	}
	return n, err
}

// parallelWrite copies src to w from off on, in blocks of blockSize with up
// to depth writes running at once. It returns the bytes written without a
// gap: all blocks up to the first failed one. A read error from src ends the
// copy after writing what was read (tusd reports a broken body as io.EOF).
func parallelWrite(w io.WriterAt, off int64, src io.Reader, blockSize, depth int) (int64, error) {
	type result struct {
		size int
		err  error
	}
	free := make(chan []byte, depth)
	for i := 0; i < depth; i++ {
		free <- make([]byte, blockSize)
	}
	var (
		wg      sync.WaitGroup
		failed  atomic.Bool
		results []*result
		readErr error
		pos     int64
	)
	for !failed.Load() {
		buf := <-free
		m, err := readBlock(src, buf)
		if m > 0 {
			r := &result{size: m}
			results = append(results, r)
			wg.Add(1)
			go func(buf []byte, at int64) {
				defer wg.Done()
				k, werr := w.WriteAt(buf[:m], at)
				if werr == nil && k < m {
					werr = io.ErrShortWrite
				}
				if werr != nil {
					r.err = werr
					failed.Store(true)
				}
				free <- buf
			}(buf, off+pos)
			pos += int64(m)
		} else {
			free <- buf
		}
		if err != nil {
			if err != io.EOF {
				readErr = err
			}
			break
		}
	}
	wg.Wait()

	var n int64
	for _, r := range results {
		if r.err != nil {
			return n, r.err
		}
		n += int64(r.size)
	}
	return n, readErr
}

// readBlock fills buf from src, short only at the end of src or on an error.
func readBlock(src io.Reader, buf []byte) (int, error) {
	m := 0
	for m < len(buf) {
		k, err := src.Read(buf[m:])
		m += k
		if err != nil {
			return m, err
		}
	}
	return m, nil
}

func (u *smbUpload) GetReader(ctx context.Context) (io.ReadCloser, error) {
	binPath := u.store.smbPath(u.info.ID)
	var f *smb2.File
	err := u.store.b.withShare(func(sh *smb2.Share) error {
		var err error
		f, err = sh.Open(binPath)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("smb tus: open data file for read %q: %w", binPath, err)
	}
	return f, nil
}

// FinishUpload updates the .info file with the final offset.
func (u *smbUpload) FinishUpload(ctx context.Context) error {
	return u.writeInfo()
}

// writeInfo rewrites the whole .info file, so retrying it after a reconnect
// is safe.
func (u *smbUpload) writeInfo() error {
	infoPath := u.store.smbPath(u.info.ID + ".info")
	data, err := json.Marshal(u.info)
	if err != nil {
		return fmt.Errorf("marshal info: %w", err)
	}
	err = u.store.b.withShare(func(sh *smb2.Share) error {
		f, err := sh.OpenFile(infoPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		if _, err := f.Write(data); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	})
	if err != nil {
		return fmt.Errorf("write info file %q: %w", infoPath, err)
	}
	return nil
}
