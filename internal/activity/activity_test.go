package activity

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStartDoneSnapshot(t *testing.T) {
	r := New()
	a := r.Start(Info{Kind: Download, File: "a.mov", Offset: 100, Total: 1000})
	b := r.Start(Info{Kind: Upload, UploadID: "u1"})

	w := httptest.NewRecorder()
	cw := a.Writer(w)
	io.WriteString(cw, "hello")
	if cw.(interface{ Unwrap() http.ResponseWriter }).Unwrap() != w {
		t.Fatal("Writer does not unwrap to the original ResponseWriter")
	}
	body := b.Body(io.NopCloser(strings.NewReader("0123456789")))
	io.ReadAll(body)

	s := r.Snapshot()
	if len(s.Running) != 2 || s.Running[0].File != "a.mov" {
		t.Fatalf("running = %+v, want a.mov first", s.Running)
	}
	if got := s.Running[0].Position(); got != 105 {
		t.Fatalf("download position = %d, want 105", got)
	}
	if got := s.Running[1].Bytes; got != 10 {
		t.Fatalf("upload bytes = %d, want 10", got)
	}

	a.Done()
	a.Done() // twice is harmless
	b.Done()
	if n := len(r.Snapshot().Running); n != 0 {
		t.Fatalf("%d still running after Done", n)
	}
}

func TestRate(t *testing.T) {
	now := time.Now()
	x := Running{Started: now.Add(-500 * time.Millisecond), Bytes: 1 << 20}
	if x.Rate(now) != 0 {
		t.Fatal("rate in the first second should be 0")
	}
	x.Started = now.Add(-2 * time.Second)
	if got := x.Rate(now); got != float64(1<<19) {
		t.Fatalf("rate = %v, want %v", got, float64(1<<19))
	}
	// An upload chunk counts with its previous chunk, so it has a speed
	// in its first second.
	x = Running{Started: now.Add(-500 * time.Millisecond), Bytes: 1 << 20, PrevBytes: 1 << 20, PrevTook: 1500 * time.Millisecond}
	if got := x.Rate(now); got != float64(1<<20) {
		t.Fatalf("rate with previous chunk = %v, want %v", got, float64(1<<20))
	}
}

// The next chunk of an upload carries the previous one; a download, another
// upload, or a chunk that ended too long ago does not.
func TestSnapshot_PreviousChunk(t *testing.T) {
	r := New()
	first := r.Start(Info{Kind: Upload, UploadID: "u1"})
	io.ReadAll(first.Body(io.NopCloser(strings.NewReader("0123456789"))))
	first.Done()

	next := r.Start(Info{Kind: Upload, UploadID: "u1"})
	other := r.Start(Info{Kind: Upload, UploadID: "u2"})
	dl := r.Start(Info{Kind: Download, UploadID: ""})
	for _, x := range r.Snapshot().Running {
		want := int64(0)
		if x.UploadID == "u1" {
			want = 10
		}
		if x.PrevBytes != want || (want > 0) != (x.PrevTook > 0) {
			t.Errorf("%s %s: previous chunk %d bytes in %v, want %d bytes", x.Kind, x.UploadID, x.PrevBytes, x.PrevTook, want)
		}
	}
	next.Done()
	other.Done()
	dl.Done()

	r.mu.Lock()
	r.prev["u1"] = chunk{bytes: 10, took: time.Second, ended: time.Now().Add(-2 * prevChunkMaxAge)}
	r.mu.Unlock()
	late := r.Start(Info{Kind: Upload, UploadID: "u1"})
	if x := r.Snapshot().Running[0]; x.PrevBytes != 0 {
		t.Errorf("a chunk that ended long ago still counts: %+v", x)
	}
	late.Done()
}

func TestConnState(t *testing.T) {
	r := New()
	c1, c2 := &net.TCPConn{}, &net.UnixConn{}
	r.ConnState(c1, http.StateNew)
	r.ConnState(c1, http.StateActive)
	r.ConnState(c2, http.StateNew)
	r.ConnState(c2, http.StateIdle)
	if s := r.Snapshot(); s.OpenConns != 2 || s.IdleConns != 1 {
		t.Fatalf("open %d idle %d, want 2 and 1", s.OpenConns, s.IdleConns)
	}
	r.ConnState(c1, http.StateClosed)
	r.ConnState(c2, http.StateHijacked)
	if s := r.Snapshot(); s.OpenConns != 0 {
		t.Fatalf("open %d after close, want 0", s.OpenConns)
	}
}
