package handler

// readAhead must serve the same bytes as the file itself, full and for Range
// requests, while reading storage in large blocks instead of ServeContent's
// 32KB steps (each step is a round trip on SMB).

import (
	"bytes"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BartVermeir/Ferri/internal/storage"
)

// countingSeeker counts reads that reach the underlying storage, and how
// many ReadAt calls run at the same time (each takes a moment, like SMB).
type countingSeeker struct {
	*bytes.Reader
	reads     atomic.Int32
	inFlight  atomic.Int32
	maxFlight atomic.Int32
	failFrom  int64 // ReadAt at or past this offset fails; 0 = never
}

var errStorage = errors.New("storage read failed")

func (c *countingSeeker) Read(p []byte) (int, error) {
	c.reads.Add(1)
	return c.Reader.Read(p)
}

func (c *countingSeeker) ReadAt(p []byte, off int64) (int, error) {
	c.reads.Add(1)
	n := c.inFlight.Add(1)
	defer c.inFlight.Add(-1)
	for {
		m := c.maxFlight.Load()
		if n <= m || c.maxFlight.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(2 * time.Millisecond)
	if c.failFrom > 0 && off >= c.failFrom {
		return 0, errStorage
	}
	return c.Reader.ReadAt(p, off)
}

// sequentialOnly hides ReadAt, for the one-block-at-a-time path.
type sequentialOnly struct{ io.ReadSeeker }

func testData(n int) []byte {
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(i * 7)
	}
	return data
}

func TestReadAhead_ServeContent(t *testing.T) {
	data := testData(3*fileReadAheadSize + 12345)
	for _, parallel := range []bool{true, false} {
		serve := func(rangeHdr string) (*httptest.ResponseRecorder, *countingSeeker) {
			src := &countingSeeker{Reader: bytes.NewReader(data)}
			var rs io.ReadSeeker = src
			if !parallel {
				rs = sequentialOnly{src}
			}
			req := httptest.NewRequest(http.MethodGet, "/f", nil)
			if rangeHdr != "" {
				req.Header.Set("Range", rangeHdr)
			}
			rr := httptest.NewRecorder()
			ra := newReadAhead(rs)
			http.ServeContent(rr, req, "f.bin", time.Time{}, ra)
			ra.Close()
			return rr, src
		}

		rr, src := serve("")
		if rr.Code != http.StatusOK || !bytes.Equal(rr.Body.Bytes(), data) {
			t.Fatalf("parallel=%v full download: status %d, %d bytes, want %d identical bytes", parallel, rr.Code, rr.Body.Len(), len(data))
		}
		// 4 blocks of data plus at most a read that finds the end.
		if n := src.reads.Load(); n > 6 {
			t.Fatalf("parallel=%v full download took %d storage reads, want at most 6", parallel, n)
		}
		if parallel && src.maxFlight.Load() < 2 {
			t.Fatalf("parallel download never had more than %d read in flight", src.maxFlight.Load())
		}
		if m := src.maxFlight.Load(); m > fileReadAheadDepth {
			t.Fatalf("%d reads in flight, want at most %d", m, fileReadAheadDepth)
		}

		rr, _ = serve("bytes=1500000-2600000")
		if rr.Code != http.StatusPartialContent || !bytes.Equal(rr.Body.Bytes(), data[1500000:2600001]) {
			t.Fatalf("parallel=%v range download: status %d, %d bytes, wrong content", parallel, rr.Code, rr.Body.Len())
		}
	}
}

// Without a SeekEnd first (the ZIP path), the end of the file is found by a
// short or empty block, also when the size is an exact number of blocks.
func TestReadAhead_ReadAllWithoutSize(t *testing.T) {
	for _, size := range []int{0, 10, fileReadAheadSize, 2 * fileReadAheadSize, 5*fileReadAheadSize + 1} {
		data := testData(size)
		src := &countingSeeker{Reader: bytes.NewReader(data)}
		ra := newReadAhead(src)
		got, err := io.ReadAll(ra)
		ra.Close()
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("size %d: read %d bytes, err %v, want %d identical bytes", size, len(got), err, size)
		}
		if src.inFlight.Load() != 0 {
			t.Fatalf("size %d: reads still running after Close", size)
		}
	}
}

// A storage error is reported after the data before it, not turned into EOF:
// a download that looks complete but is cut short must fail.
func TestReadAhead_StorageErrorIsReported(t *testing.T) {
	data := testData(4 * fileReadAheadSize)
	src := &countingSeeker{Reader: bytes.NewReader(data), failFrom: 2 * fileReadAheadSize}
	ra := newReadAhead(src)
	got, err := io.ReadAll(ra)
	ra.Close()
	if !errors.Is(err, errStorage) {
		t.Fatalf("err = %v, want the storage error", err)
	}
	if !bytes.Equal(got, data[:2*fileReadAheadSize]) {
		t.Fatalf("read %d bytes before the error, want the %d before the failing block", len(got), 2*fileReadAheadSize)
	}
}

func TestReadAhead_SeekAndEOF(t *testing.T) {
	data := []byte("0123456789")
	for _, parallel := range []bool{true, false} {
		var src io.ReadSeeker = bytes.NewReader(data)
		if !parallel {
			src = sequentialOnly{src}
		}
		r := newReadAhead(src)
		if end, err := r.Seek(0, io.SeekEnd); err != nil || end != 10 {
			t.Fatalf("parallel=%v seek end: %d, %v", parallel, end, err)
		}
		if _, err := r.Seek(7, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(r)
		if err != nil || string(got) != "789" {
			t.Fatalf("parallel=%v read after seek: %q, %v", parallel, got, err)
		}
		if _, err := r.Seek(-4, io.SeekCurrent); err != nil {
			t.Fatal(err)
		}
		got, _ = io.ReadAll(r)
		if string(got) != "6789" {
			t.Fatalf("parallel=%v read after seek back: %q", parallel, got)
		}
		if _, err := r.Seek(2, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		got, _ = io.ReadAll(r)
		if string(got) != "23456789" {
			t.Fatalf("parallel=%v read after seek before the block: %q", parallel, got)
		}
		r.Close()
	}
}

// Seeking around a large file while blocks are in flight drops them and
// reads from the new place.
func TestReadAhead_SeekWhileReading(t *testing.T) {
	data := testData(6 * fileReadAheadSize)
	src := &countingSeeker{Reader: bytes.NewReader(data)}
	ra := newReadAhead(src)
	defer ra.Close()
	p := make([]byte, 1000)
	for _, off := range []int64{0, 3*fileReadAheadSize + 17, 100, 5*fileReadAheadSize - 10} {
		if _, err := ra.Seek(off, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		n, err := io.ReadFull(ra, p)
		if err != nil || !bytes.Equal(p[:n], data[off:off+int64(n)]) {
			t.Fatalf("at %d: %d bytes, err %v, wrong content", off, n, err)
		}
	}
}

// openStored reads the flat TUS file first and falls back to storage_path.
func TestOpenStored_TUSPathFirst(t *testing.T) {
	root := t.TempDir()
	mgr := storage.NewManager(storage.NewLocalBackend(root))
	if err := os.MkdirAll(filepath.Join(root, "transfers", "t1"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, content string) {
		if err := os.WriteFile(filepath.Join(root, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(storagePath string, tusID sql.NullString) string {
		t.Helper()
		f, err := openStored(mgr, storagePath, tusID)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, ok := f.(io.ReaderAt); !ok {
			t.Fatal("local file is not an io.ReaderAt: downloads would read one block at a time")
		}
		b, _ := io.ReadAll(f)
		return string(b)
	}
	write("transfers/t1/f1", "legacy")
	write("tus-uuid", "tus")

	if got := read("transfers/t1/f1", sql.NullString{String: "tus-uuid", Valid: true}); got != "tus" {
		t.Fatalf("both present: read %q, want the TUS file", got)
	}
	if got := read("transfers/t1/f1", sql.NullString{String: "gone", Valid: true}); got != "legacy" {
		t.Fatalf("TUS file missing: read %q, want the storage_path fallback", got)
	}
	if got := read("transfers/t1/f1", sql.NullString{}); got != "legacy" {
		t.Fatalf("no tus_upload_id: read %q, want storage_path", got)
	}
}
