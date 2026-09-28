// Package activity keeps, in memory, what the app is moving right now: every
// running download and every upload chunk (TUS PATCH), with the bytes so far,
// and the open HTTP connections. The admin dashboard shows it, so an admin
// sees what is going on and what a deploy is waiting for (scripts/deploy.sh
// only sees connections, not which link they belong to).
//
// Nothing is stored here: a restart starts from zero, and a finished
// transfer leaves no trace. OnDone hands every finished transfer to whoever
// keeps statistics (main.go stores the downloads).
package activity

import (
	"io"
	"net"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	Upload   = "upload"
	Download = "download"
)

// Info describes one running transfer of bytes. For an upload only UploadID,
// Offset and IP are known when it starts; the dashboard looks up the rest.
type Info struct {
	Kind     string // Upload or Download
	Item     string // "transfer" or "request"
	ItemID   string
	Title    string
	File     string // file name, or "All files (ZIP)"
	Who      string // recipient address, "requester", "admin"
	IP       string
	UploadID string // tusd upload ID (uploads)
	Offset   int64  // position the bytes start at: Range start, Upload-Offset
	Total    int64  // size of the file or ZIP, 0 = unknown
}

// Handle is one registered transfer. Done must be called when it ends,
// also when the handler panics (defer it).
type Handle struct {
	info    Info
	started time.Time
	bytes   atomic.Int64
	reg     *Registry
	id      uint64
	done    atomic.Bool
}

// Registry holds the running transfers and the connection states.
type Registry struct {
	mu      sync.Mutex
	next    uint64
	running map[uint64]*Handle
	conns   map[net.Conn]http.ConnState

	// OnDone, when set, runs once for every transfer that ends, with how
	// long it took, on the goroutine of the request. Set it before serving.
	OnDone func(x Running, took time.Duration)
}

// Default is the registry the handlers and the server use.
var Default = New()

func New() *Registry {
	return &Registry{running: map[uint64]*Handle{}, conns: map[net.Conn]http.ConnState{}}
}

// Start registers a transfer that begins now.
func (r *Registry) Start(info Info) *Handle {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	h := &Handle{info: info, started: time.Now(), reg: r, id: r.next}
	r.running[h.id] = h
	return h
}

// Done removes the transfer and hands it to OnDone. Calling it twice is
// harmless.
func (h *Handle) Done() {
	if h.done.Swap(true) {
		return
	}
	h.reg.mu.Lock()
	delete(h.reg.running, h.id)
	h.reg.mu.Unlock()
	if h.reg.OnDone != nil {
		h.reg.OnDone(Running{Info: h.info, Started: h.started, Bytes: h.bytes.Load()}, time.Since(h.started))
	}
}

func (h *Handle) add(n int) {
	if n > 0 {
		h.bytes.Add(int64(n))
	}
}

// Writer counts what the handler writes to the client (downloads).
func (h *Handle) Writer(w http.ResponseWriter) http.ResponseWriter {
	return &countingWriter{ResponseWriter: w, h: h}
}

// Body counts what is read from the request body (uploads).
func (h *Handle) Body(body io.ReadCloser) io.ReadCloser {
	return &countingBody{body: body, h: h}
}

type countingWriter struct {
	http.ResponseWriter
	h *Handle
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.h.add(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the connection (deadlines).
func (c *countingWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

type countingBody struct {
	body io.ReadCloser
	h    *Handle
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.body.Read(p)
	c.h.add(n)
	return n, err
}

func (c *countingBody) Close() error { return c.body.Close() }

// ConnState is the http.Server hook that keeps the open connections.
func (r *Registry) ConnState(c net.Conn, s http.ConnState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s == http.StateClosed || s == http.StateHijacked {
		delete(r.conns, c)
		return
	}
	r.conns[c] = s
}

// Running is a snapshot of one transfer.
type Running struct {
	Info
	Started time.Time
	Bytes   int64 // bytes moved on this connection
}

// Position is where the transfer is in the file: Offset plus what moved.
func (x Running) Position() int64 { return x.Offset + x.Bytes }

// Rate is the average speed on this connection in bytes per second, 0 in
// the first second (too little to say anything).
func (x Running) Rate(now time.Time) float64 {
	d := now.Sub(x.Started).Seconds()
	if d < 1 {
		return 0
	}
	return float64(x.Bytes) / d
}

// Snapshot is the state at one moment.
type Snapshot struct {
	Taken     time.Time
	Running   []Running // oldest first
	OpenConns int       // open connections to the app
	IdleConns int       // of those, waiting for a next request (keep-alive)
}

func (r *Registry) Snapshot() Snapshot {
	r.mu.Lock()
	s := Snapshot{Taken: time.Now()}
	for _, h := range r.running {
		s.Running = append(s.Running, Running{Info: h.info, Started: h.started, Bytes: h.bytes.Load()})
	}
	for _, st := range r.conns {
		s.OpenConns++
		if st == http.StateIdle {
			s.IdleConns++
		}
	}
	r.mu.Unlock()
	sort.Slice(s.Running, func(i, j int) bool { return s.Running[i].Started.Before(s.Running[j].Started) })
	return s
}
