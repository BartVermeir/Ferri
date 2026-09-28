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
