package storage

// parallelWrite and resumeOffset carry the SMB upload's correctness: blocks
// are written in parallel and may land out of order, so the offset it reports
// (and an upload later resumes from) must only cover bytes written without a
// gap. go-smb2 needs a real share, so these run on an in-memory WriterAt.

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// memFile is an io.WriterAt in memory. Each write takes a random moment, so
// parallel blocks finish out of order. A write at or past failFrom fails.
type memFile struct {
	mu        sync.Mutex
	data      []byte
	inFlight  atomic.Int32
	maxFlight atomic.Int32
	failFrom  int64 // 0 = never
}

var errWrite = errors.New("write failed")

func (f *memFile) WriteAt(p []byte, off int64) (int, error) {
	n := f.inFlight.Add(1)
	defer f.inFlight.Add(-1)
	for {
		m := f.maxFlight.Load()
		if n <= m || f.maxFlight.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(time.Duration(rand.Intn(3)) * time.Millisecond)
	if f.failFrom > 0 && off >= f.failFrom {
		return -1, errWrite // like go-smb2, which returns -1 on an error
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if end := off + int64(len(p)); end > int64(len(f.data)) {
		f.data = append(f.data, make([]byte, end-int64(len(f.data)))...)
	}
	copy(f.data[off:], p)
	return len(p), nil
}

func patterned(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + i/1000)
	}
	return b
}

const testBlock = 1000

func TestParallelWrite_AllSizes(t *testing.T) {
	for _, size := range []int{0, 1, testBlock - 1, testBlock, 4 * testBlock, 7*testBlock + 3} {
		for _, depth := range []int{1, 4} {
			data := patterned(size)
			f := &memFile{}
			n, err := parallelWrite(f, 0, bytes.NewReader(data), testBlock, depth)
			if err != nil || n != int64(size) {
				t.Fatalf("size %d depth %d: n = %d, err = %v", size, depth, n, err)
			}
			if !bytes.Equal(f.data, data) {
				t.Fatalf("size %d depth %d: file content differs", size, depth)
			}
			if m := f.maxFlight.Load(); m > int32(depth) {
				t.Fatalf("size %d depth %d: %d writes at once", size, depth, m)
			}
		}
	}
}

// Writes land at the chunk's offset, after what is already there.
func TestParallelWrite_AtOffset(t *testing.T) {
	head, tail := patterned(2500), patterned(3700)
	f := &memFile{data: append([]byte(nil), head...)}
	n, err := parallelWrite(f, int64(len(head)), bytes.NewReader(tail), testBlock, 4)
	if err != nil || n != int64(len(tail)) {
		t.Fatalf("n = %d, err = %v", n, err)
	}
	if !bytes.Equal(f.data, append(head, tail...)) {
		t.Fatal("chunk not written right after the existing data")
	}
}

func TestParallelWrite_ReallyParallel(t *testing.T) {
	f := &memFile{}
	if _, err := parallelWrite(f, 0, bytes.NewReader(patterned(40*testBlock)), testBlock, 4); err != nil {
		t.Fatal(err)
	}
	if f.maxFlight.Load() < 2 {
		t.Fatalf("at most %d write at once, want parallel writes", f.maxFlight.Load())
	}
}

// A failed block ends the chunk. The reported bytes stop before it, even
// when later blocks were written: resuming there must not skip a gap.
func TestParallelWrite_FailureReportsOnlyGapFree(t *testing.T) {
	for _, depth := range []int{1, 4} {
		data := patterned(10 * testBlock)
		f := &memFile{failFrom: 3 * testBlock}
		n, err := parallelWrite(f, 0, bytes.NewReader(data), testBlock, depth)
		if !errors.Is(err, errWrite) {
			t.Fatalf("depth %d: err = %v, want the write error", depth, err)
		}
		if n != 3*testBlock {
			t.Fatalf("depth %d: n = %d, want %d (the blocks before the failed one)", depth, n, 3*testBlock)
		}
		if !bytes.Equal(f.data[:n], data[:n]) {
			t.Fatalf("depth %d: the reported bytes are not what was sent", depth)
		}
	}
}

// A short write without an error is still a failed block.
type shortWriter struct{ memFile }

func (s *shortWriter) WriteAt(p []byte, off int64) (int, error) {
	if off >= 2*testBlock {
		return len(p) / 2, nil
	}
	return s.memFile.WriteAt(p, off)
}

func TestParallelWrite_ShortWriteIsFailure(t *testing.T) {
	n, err := parallelWrite(&shortWriter{}, 0, bytes.NewReader(patterned(5*testBlock)), testBlock, 4)
	if !errors.Is(err, io.ErrShortWrite) || n != 2*testBlock {
		t.Fatalf("n = %d, err = %v, want %d and io.ErrShortWrite", n, err, 2*testBlock)
	}
}

// A body that ends early (tusd hands a broken body over as a plain end) keeps
// what arrived; a real read error is returned after writing what was read.
type brokenReader struct {
	r   io.Reader
	err error
}

func (b *brokenReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err == io.EOF {
		return n, b.err
	}
	return n, err
}

func TestParallelWrite_ReadErrorKeepsWhatArrived(t *testing.T) {
	data := patterned(2*testBlock + 500)
	readErr := errors.New("connection reset")
	f := &memFile{}
	n, err := parallelWrite(f, 0, &brokenReader{r: bytes.NewReader(data), err: readErr}, testBlock, 4)
	if !errors.Is(err, readErr) || n != int64(len(data)) {
		t.Fatalf("n = %d, err = %v, want %d and the read error", n, err, len(data))
	}
	if !bytes.Equal(f.data, data) {
		t.Fatal("the bytes that arrived were not all written")
	}
}

func TestResumeOffset(t *testing.T) {
	cases := []struct {
		name                       string
		tracked                    bool
		recorded, size, infoOffset int64
		want                       int64
	}{
		{"tracked: recorded offset, file longer after a failed block", true, 3000, 7000, 0, 3000},
		{"tracked: never past the end of the data", true, 5000, 4000, 0, 4000},
		{"tracked: size unknown", true, 3000, -1, 0, 3000},
		{"tracked: no offset file yet", true, 0, 0, 0, 0},
		{"untracked (created before): data file size", false, 0, 13474201600, 0, 13474201600},
		{"untracked, size unknown: .info offset", false, 0, -1, 1200, 1200},
	}
	for _, c := range cases {
		if got := resumeOffset(c.tracked, c.recorded, c.size, c.infoOffset); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}
