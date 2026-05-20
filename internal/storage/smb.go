package storage

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path"
	"strings"
	"sync"

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

// SMBBackend stores files on a remote SMB/CIFS share using a pure-Go SMB2 client.
// No OS-level mounting or elevated privileges are required.
type SMBBackend struct {
	cfg     SMBConfig
	mu      sync.RWMutex
	session *smb2.Session
	share   *smb2.Share
	conn    net.Conn
}

// NewSMBBackend connects to the SMB share and returns a ready Backend.
func NewSMBBackend(cfg SMBConfig) (*SMBBackend, error) {
	b := &SMBBackend{cfg: cfg}
	if err := b.connect(); err != nil {
		return nil, err
	}
	slog.Info("storage: SMB connected", "host", cfg.Host, "share", cfg.Share)
	return b, nil
}

// connect (re)establishes the TCP + SMB2 session and mounts the share.
// Must be called with b.mu held for writing, or during construction.
func (b *SMBBackend) connect() error {
	// Close any existing connection silently
	b.disconnectLocked()

	conn, err := net.Dial("tcp", b.cfg.Host+":445")
	if err != nil {
		return fmt.Errorf("smb: TCP connect to %s:445: %w", b.cfg.Host, err)
	}

	d := &smb2.Dialer{
		Initiator: &smb2.NTLMInitiator{
			User:     b.cfg.Username,
			Password: b.cfg.Password,
			Domain:   b.cfg.Domain,
		},
	}
	session, err := d.Dial(conn)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smb: negotiate session on %s: %w", b.cfg.Host, err)
	}

	share, err := session.Mount(b.cfg.Share)
	if err != nil {
		session.Logoff()
		conn.Close()
		return fmt.Errorf("smb: mount share %q on %s: %w", b.cfg.Share, b.cfg.Host, err)
	}

	b.conn = conn
	b.session = session
	b.share = share
	return nil
}

// disconnectLocked tears down the current connection without acquiring the lock.
func (b *SMBBackend) disconnectLocked() {
	if b.share != nil {
		b.share.Umount()
		b.share = nil
	}
	if b.session != nil {
		b.session.Logoff()
		b.session = nil
	}
	if b.conn != nil {
		b.conn.Close()
		b.conn = nil
	}
}

// withShare executes fn with the current share. If fn returns a connection
// error it reconnects once and retries.
func (b *SMBBackend) withShare(fn func(*smb2.Share) error) error {
	b.mu.RLock()
	share := b.share
	b.mu.RUnlock()

	err := fn(share)
	if err == nil {
		return nil
	}

	// Only reconnect on probable connection-level errors
	if !isConnectionError(err) {
		return err
	}

	slog.Warn("storage: SMB connection error, reconnecting", "error", err)
	b.mu.Lock()
	reconnErr := b.connect()
	newShare := b.share
	b.mu.Unlock()

	if reconnErr != nil {
		return fmt.Errorf("smb reconnect failed: %w (original: %v)", reconnErr, err)
	}
	return fn(newShare)
}

// isConnectionError reports whether err looks like a network/connection error.
func isConnectionError(err error) bool {
	if err == nil {
		return false
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
func (b *SMBBackend) smbPath(relPath string) string {
	base := strings.Trim(b.cfg.BasePath, "/")
	rel := strings.TrimPrefix(relPath, "/")
	if base == "" {
		return rel
	}
	return base + "/" + rel
}

// ── Backend interface ─────────────────────────────────────────────────────────

// Open opens a file for reading. *smb2.File implements io.ReadSeekCloser.
func (b *SMBBackend) Open(relPath string) (io.ReadSeekCloser, error) {
	var f io.ReadSeekCloser
	err := b.withShare(func(s *smb2.Share) error {
		var err error
		f, err = s.Open(b.smbPath(relPath))
		return err
	})
	return f, err
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

// TUSStore returns a tusd.DataStore backed by this SMB share.
// The store is constructed fresh each call but shares the same connection.
func (b *SMBBackend) TUSStore() tusd.DataStore {
	b.mu.RLock()
	share := b.share
	b.mu.RUnlock()
	basePath := strings.Trim(b.cfg.BasePath, "/")
	return newSMBTUSStore(share, basePath)
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

// Close unmounts the share and closes the session and TCP connection.
func (b *SMBBackend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.disconnectLocked()
	return nil
}

// isNotExist reports whether err indicates a missing file/directory.
func isNotExist(err error) bool {
	if err == nil {
		return false
	}
	return os.IsNotExist(err) || strings.Contains(strings.ToLower(err.Error()), "no such file")
}
