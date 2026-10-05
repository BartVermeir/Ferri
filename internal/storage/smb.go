package storage

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	smb2 "github.com/hirochachacha/go-smb2"
	tusd "github.com/tus/tusd/v2/pkg/handler"
)

// SMBConfig holds all parameters needed to connect to an SMB share.
type SMBConfig struct {
	Host     string // hostname or IP address (without \\ prefix)
	Share    string // share name (e.g. "ferri", not "\\host\ferri")
	BasePath string // path within the share (e.g. "" or "uploads")
	Username string
	Password string
	Domain   string // optional; leave empty for workgroup or Azure AD UPN
}

// smbMaxCredits caps the SMB credits each connection holds, and so how much
// can be in flight on it: a credit covers 64KB, so 512 = 32MB (go-smb2's
// default is 128). Uploads keep 4 blocks of 1MB in flight per connection
// (writeDepth), downloads 2 (handler.fileReadAheadDepth, 8 over the
// connections). The server may grant fewer: then less is in flight.
const smbMaxCredits = 512

// smbConnections is how many SMB connections the backend opens to the share.
// The share limits the throughput of each connection. A file's data is
// striped over all of them (stripedFile); the first connection also carries
// all small operations.
const smbConnections = 4

// SMBBackend stores files on a remote SMB/CIFS share using a pure-Go SMB2 client.
// No OS-level mounting or elevated privileges are required.
type SMBBackend struct {
	cfg   SMBConfig
	conns []*smbConn
}

// smbConn is one connection (TCP + SMB session + mounted share) with its own
// reconnect.
type smbConn struct {
	mu      sync.RWMutex
	session *smb2.Session
	share   *smb2.Share
	conn    net.Conn
	warned  atomic.Bool // an extra connection failed and was logged; reset when it connects
}

// NewSMBBackend connects to the SMB share and returns a ready Backend. Only
// the first connection is opened here; the others open on first use, so a
// connection test logs in once.
func NewSMBBackend(cfg SMBConfig) (*SMBBackend, error) {
	b := &SMBBackend{cfg: cfg, conns: make([]*smbConn, smbConnections)}
	for i := range b.conns {
		b.conns[i] = &smbConn{}
	}
	if err := b.conns[0].connect(cfg); err != nil {
		return nil, err
	}
	slog.Info("storage: SMB connected", "host", cfg.Host, "share", cfg.Share, "connections", smbConnections)
	return b, nil
}

// connect (re)establishes the TCP + SMB2 session and mounts the share.
// Must be called with c.mu held for writing, or during construction.
func (c *smbConn) connect(cfg SMBConfig) error {
	// Close any existing connection silently
	c.disconnectLocked()

	// The timeout makes an unreachable host fail in seconds; keepalives
	// detect a connection the server dropped silently.
	dialer := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	conn, err := dialer.Dial("tcp", cfg.Host+":445")
	if err != nil {
		return fmt.Errorf("smb: TCP connect to %s:445: %w", cfg.Host, err)
	}

	d := &smb2.Dialer{
		MaxCreditBalance: smbMaxCredits,
		Initiator: &smb2.NTLMInitiator{
			User:     cfg.Username,
			Password: cfg.Password,
			Domain:   cfg.Domain,
		},
	}
	session, err := d.Dial(conn)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smb: negotiate session on %s: %w", cfg.Host, err)
	}

	share, err := session.Mount(cfg.Share)
	if err != nil {
		session.Logoff()
		conn.Close()
		return fmt.Errorf("smb: mount share %q on %s: %w", cfg.Share, cfg.Host, err)
	}

	c.conn = conn
	c.session = session
	c.share = share
	return nil
}

// disconnectLocked tears down the current connection without acquiring the lock.
func (c *smbConn) disconnectLocked() {
	if c.share != nil {
		c.share.Umount()
		c.share = nil
	}
	if c.session != nil {
		c.session.Logoff()
		c.session = nil
	}
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}

// withShare executes fn on the first connection, reconnecting once on a
// connection error (withConn).
func (b *SMBBackend) withShare(fn func(*smb2.Share) error) error {
	return b.withConn(0, fn)
}

// withConn executes fn with connection i's share. If fn returns a connection
// error it reconnects once and retries. A connection not opened yet (the
// extra ones, until first use) is opened first.
func (b *SMBBackend) withConn(i int, fn func(*smb2.Share) error) error {
	c := b.conns[i]
	c.mu.RLock()
	share := c.share
	c.mu.RUnlock()

	var err error
	if share == nil {
		// Not opened yet, or a failed reconnect left no share: (re)connect
		// before calling fn.
		err = errors.New("smb: not connected")
	} else {
		if err = fn(share); err == nil {
			return nil
		}
		// Reconnect only on connection-level errors
		if !isConnectionError(err) {
			return err
		}
	}

	c.mu.Lock()
	var reconnErr error
	if c.share == share {
		// Only the first goroutine to notice reconnects; parallel uploads
		// hit the same dead session and use the share it opened.
		if share != nil {
			slog.Warn("storage: SMB connection error, reconnecting", "connection", i, "error", err)
		}
		reconnErr = c.connect(b.cfg)
		if reconnErr == nil {
			c.warned.Store(false)
		}
	}
	newShare := c.share
	c.mu.Unlock()

	if reconnErr != nil {
		return fmt.Errorf("smb reconnect failed: %w (original: %v)", reconnErr, err)
	}
	return fn(newShare)
}

// openStriped opens one file on every connection, for data that is read or
// written in parallel blocks. The first connection's open decides: its error
// (a missing file, say) is returned. An extra connection that fails is left
// out, logged once until it works again: the transfer goes on with fewer.
func (b *SMBBackend) openStriped(open func(*smb2.Share) (*smb2.File, error)) (*stripedFile, error) {
	files := make([]smbHandle, 0, len(b.conns))
	for i := range b.conns {
		var f *smb2.File
		err := b.withConn(i, func(sh *smb2.Share) error {
			var err error
			f, err = open(sh)
			return err
		})
		if err != nil {
			if i == 0 {
				return nil, err
			}
			if !b.conns[i].warned.Swap(true) {
				slog.Warn("storage: extra SMB connection unavailable, using fewer", "connection", i, "error", err)
			}
			continue
		}
		files = append(files, f)
	}
	return &stripedFile{files: files}, nil
}

// stripeBlock is the unit a striped file hands to one connection: the block
// size of downloads (handler.fileReadAheadSize) and uploads (writeBufSize).
const stripeBlock = 1 << 20 // 1MB

// smbHandle is what stripedFile needs of an open file; *smb2.File has it all.
type smbHandle interface {
	io.ReadSeeker
	io.ReaderAt
	io.WriterAt
	io.Closer
}

// stripedFile is one file open on several connections. ReadAt and WriteAt
// send block k (offset / stripeBlock) over connection k mod n, so parallel
// blocks spread over all connections. Read and Seek use the first one.
type stripedFile struct {
	files []smbHandle
}

func (f *stripedFile) pick(off int64) smbHandle {
	return f.files[int((off/stripeBlock)%int64(len(f.files)))]
}

func (f *stripedFile) ReadAt(p []byte, off int64) (int, error)  { return f.pick(off).ReadAt(p, off) }
func (f *stripedFile) WriteAt(p []byte, off int64) (int, error) { return f.pick(off).WriteAt(p, off) }
func (f *stripedFile) Read(p []byte) (int, error)               { return f.files[0].Read(p) }
func (f *stripedFile) Seek(off int64, whence int) (int64, error) {
	return f.files[0].Seek(off, whence)
}

// Close closes the file on every connection and returns the first error.
func (f *stripedFile) Close() error {
	var first error
	for _, h := range f.files {
		if err := h.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// NT status codes with which an SMB server says the session or share is
// gone. Their texts ("The client session has expired", ...) contain none of
// the keywords below, so they are matched by code.
var sessionGoneCodes = map[uint32]bool{
	0xC000035C: true, // STATUS_NETWORK_SESSION_EXPIRED
	0xC0000203: true, // STATUS_USER_SESSION_DELETED
	0xC00000C9: true, // STATUS_NETWORK_NAME_DELETED
}

// isConnectionError reports whether err looks like a network/connection
// error, or an SMB session the server ended: both are fixed by reconnecting.
func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	var re *smb2.ResponseError
	if errors.As(err, &re) && sessionGoneCodes[re.Code] {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, kw := range []string{"connection", "reset", "broken", "eof", "closed", "refused", "timeout"} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}

// smbPath returns the full path on the share for a given relative path.
// path.Clean with a leading slash collapses any "." / ".." segments and cannot
// escape above root, so the result always stays within BasePath.
func (b *SMBBackend) smbPath(relPath string) string {
	base := strings.Trim(b.cfg.BasePath, "/")
	rel := strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(relPath, "/")), "/")
	if base == "" {
		return rel
	}
	if rel == "" {
		return base
	}
	return base + "/" + rel
}

// ── Backend interface ─────────────────────────────────────────────────────────

// Open opens a file for reading, on every connection (stripedFile), so a
// download reading ahead in parallel blocks uses all of them.
func (b *SMBBackend) Open(relPath string) (io.ReadSeekCloser, error) {
	p := b.smbPath(relPath)
	f, err := b.openStriped(func(s *smb2.Share) (*smb2.File, error) { return s.Open(p) })
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Stat returns FileInfo for the given relative path.
func (b *SMBBackend) Stat(relPath string) (os.FileInfo, error) {
	var fi os.FileInfo
	err := b.withShare(func(s *smb2.Share) error {
		var err error
		fi, err = s.Stat(b.smbPath(relPath))
		return err
	})
	return fi, err
}

// Remove deletes a single file.
func (b *SMBBackend) Remove(relPath string) error {
	return b.withShare(func(s *smb2.Share) error {
		err := s.Remove(b.smbPath(relPath))
		if isNotExist(err) {
			return nil
		}
		return err
	})
}

// RemoveAll deletes a directory and all its contents.
func (b *SMBBackend) RemoveAll(relPath string) error {
	return b.withShare(func(s *smb2.Share) error {
		err := s.RemoveAll(b.smbPath(relPath))
		if isNotExist(err) {
			return nil
		}
		return err
	})
}

// MkdirAll creates a directory path and all parents.
func (b *SMBBackend) MkdirAll(relPath string) error {
	return b.withShare(func(s *smb2.Share) error {
		return s.MkdirAll(b.smbPath(relPath), 0o755)
	})
}

// TUSStore returns a tusd.DataStore backed by this SMB share. It goes
// through withShare like every other operation, so a dropped or expired
// session is re-established for uploads too.
func (b *SMBBackend) TUSStore() tusd.DataStore {
	return newSMBTUSStore(b, strings.Trim(b.cfg.BasePath, "/"))
}

// FreeSpace returns the bytes available to this user on the share
// (the caller-available units, which respect quotas). go-smb2's
// BlockSize is the sector size and FragmentSize the sectors per allocation
// unit, so one unit is BlockSize × FragmentSize bytes.
func (b *SMBBackend) FreeSpace() (uint64, error) {
	var free uint64
	err := b.withShare(func(s *smb2.Share) error {
		fi, err := s.Statfs(b.smbPath(""))
		if err != nil {
			return err
		}
		free = fi.AvailableBlockCount() * fi.FragmentSize() * fi.BlockSize()
		return nil
	})
	return free, err
}

// TestConnection checks connectivity and write access.
func (b *SMBBackend) TestConnection() error {
	probeRel := path.Join(strings.Trim(b.cfg.BasePath, "/"), ".ferri-probe")
	probeRel = strings.TrimPrefix(probeRel, "/")

	return b.withShare(func(s *smb2.Share) error {
		probePath := b.smbPath(".ferri-probe")

		// Ensure base path directory exists
		if base := strings.Trim(b.cfg.BasePath, "/"); base != "" {
			if err := s.MkdirAll(base, 0o755); err != nil {
				return fmt.Errorf("cannot create base path %q: %w", base, err)
			}
		}

		// Write probe file
		f, err := s.OpenFile(probePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return fmt.Errorf("share not writable: %w", err)
		}
		f.Write([]byte("ferri"))
		f.Close()

		// Remove probe file
		s.Remove(probePath)
		_ = probeRel
		return nil
	})
}

// Type returns "smb".
func (b *SMBBackend) Type() string { return "smb" }

// Close unmounts the share and closes the sessions and TCP connections.
func (b *SMBBackend) Close() error {
	for _, c := range b.conns {
		c.mu.Lock()
		c.disconnectLocked()
		c.mu.Unlock()
	}
	return nil
}

// isNotExist reports whether err indicates a missing file/directory.
func isNotExist(err error) bool {
	if err == nil {
		return false
	}
	return os.IsNotExist(err) || strings.Contains(strings.ToLower(err.Error()), "no such file")
}
