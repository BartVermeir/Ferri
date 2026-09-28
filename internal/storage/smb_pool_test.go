package storage

// stripedFile spreads one file's blocks over several SMB connections. A real
// share is not available in tests, so the connections are fake handles over
// one shared in-memory file.

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
)

// fakeHandle is one connection's handle on a shared file. It records which
// offsets it served, and fails every write when fail is set.
type fakeHandle struct {
	mem     *memFile
	src     *bytes.Reader
	mu      sync.Mutex
	offs    []int64
	fail    bool
	closed  bool
	closeEr error
}

func (h *fakeHandle) note(off int64) {
	h.mu.Lock()
	h.offs = append(h.offs, off)
	h.mu.Unlock()
}

func (h *fakeHandle) ReadAt(p []byte, off int64) (int, error) {
	h.note(off)
	return h.src.ReadAt(p, off)
}

func (h *fakeHandle) WriteAt(p []byte, off int64) (int, error) {
	h.note(off)
	if h.fail {
		return -1, errWrite
	}
	return h.mem.WriteAt(p, off)
}

func (h *fakeHandle) Read(p []byte) (int, error)                { return h.src.Read(p) }
func (h *fakeHandle) Seek(off int64, whence int) (int64, error) { return h.src.Seek(off, whence) }
func (h *fakeHandle) Close() error                              { h.closed = true; return h.closeEr }

func striped(n int, data []byte) (*stripedFile, []*fakeHandle, *memFile) {
	mem := &memFile{}
	src := bytes.NewReader(data)
	sf := &stripedFile{}
	var hs []*fakeHandle
	for i := 0; i < n; i++ {
		h := &fakeHandle{mem: mem, src: src}
		hs = append(hs, h)
		sf.files = append(sf.files, h)
	}
	return sf, hs, mem
}

// Consecutive blocks go to consecutive connections, also when the blocks do
// not start on a block boundary (a download resumed mid-file).
func TestStripedFile_BlocksRotateOverConnections(t *testing.T) {
	data := patterned(10 * stripeBlock)
	for _, base := range []int64{0, 12345} {
		sf, hs, _ := striped(3, data)
		p := make([]byte, 100)
		for k := int64(0); k < 6; k++ {
			if _, err := sf.ReadAt(p, base+k*stripeBlock); err != nil {
				t.Fatal(err)
			}
		}
		for i, h := range hs {
			if len(h.offs) != 2 {
				t.Fatalf("base %d: connection %d served %d blocks, want 2 of 6", base, i, len(h.offs))
			}
			if h.offs[1]-h.offs[0] != 3*stripeBlock {
				t.Fatalf("base %d: connection %d served %v, want every 3rd block", base, i, h.offs)
			}
		}
	}
}

// An upload chunk striped over the connections ends up whole and in place,
// with every connection taking part.
func TestStripedFile_ParallelWriteOverConnections(t *testing.T) {
	data := patterned(9*stripeBlock + 77)
	sf, hs, mem := striped(4, nil)
	n, err := parallelWrite(sf, 0, bytes.NewReader(data), stripeBlock, 8)
	if err != nil || n != int64(len(data)) {
		t.Fatalf("n = %d, err = %v", n, err)
	}
	if !bytes.Equal(mem.data, data) {
		t.Fatal("file content differs")
	}
	for i, h := range hs {
		if len(h.offs) == 0 {
			t.Fatalf("connection %d wrote nothing", i)
		}
	}
}

// One connection failing ends the chunk at the first of its blocks: the
// reported bytes stop there, whatever the other connections wrote after it.
func TestStripedFile_OneConnectionFails(t *testing.T) {
	data := patterned(12 * stripeBlock)
	sf, hs, mem := striped(4, nil)
	hs[2].fail = true // gets blocks 2, 6, 10
	n, err := parallelWrite(sf, 0, bytes.NewReader(data), stripeBlock, 8)
	if !errors.Is(err, errWrite) {
		t.Fatalf("err = %v, want the write error", err)
	}
	if n != 2*stripeBlock {
		t.Fatalf("n = %d, want %d (blocks 0 and 1, before the failed block 2)", n, 2*stripeBlock)
	}
	if !bytes.Equal(mem.data[:n], data[:n]) {
		t.Fatal("the reported bytes are not what was sent")
	}
}

// Read and Seek use the first connection; Close closes every connection's
// handle and reports the first error.
func TestStripedFile_ReadSeekClose(t *testing.T) {
	data := patterned(3000)
	sf, hs, _ := striped(3, data)
	if end, err := sf.Seek(0, io.SeekEnd); err != nil || end != 3000 {
		t.Fatalf("seek end = %d, %v", end, err)
	}
	if _, err := sf.Seek(1000, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(sf)
	if err != nil || !bytes.Equal(got, data[1000:]) {
		t.Fatalf("read after seek: %d bytes, %v", len(got), err)
	}
	boom := errors.New("close failed")
	hs[1].closeEr = boom
	if err := sf.Close(); !errors.Is(err, boom) {
		t.Fatalf("close = %v, want the first handle error", err)
	}
	for i, h := range hs {
		if !h.closed {
			t.Fatalf("connection %d handle not closed", i)
		}
	}
}
