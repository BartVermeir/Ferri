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
	"os"

	"github.com/google/uuid"
	smb2 "github.com/hirochachacha/go-smb2"
	tusd "github.com/tus/tusd/v2/pkg/handler"
)

// smbTUSStore is a tusd.DataStore backed by an SMB share. Every share
// operation goes through the backend's withShare, which reconnects once when
// the connection or session is gone (audit M6). It used to hold the raw share
// pointer, so after the server dropped an idle session every upload failed
// until some download or delete happened to trigger a reconnect.
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

	// Write .info file
	upload := &smbUpload{store: s, info: info}
	if err := upload.writeInfo(); err != nil {
		return nil, fmt.Errorf("smb tus: write info file: %w", err)
	}

	return upload, nil
}

// GetUpload retrieves an existing TUS upload from the SMB share. Only a
// missing .info file is ErrNotFound; any other error is returned as is. Before,
// every error was "not found", so after a network hiccup the browser dropped
// an upload that was still there instead of retrying it (audit M6).
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

	// Sync offset from actual data file size (mirrors filestore behaviour).
	if size >= 0 {
		info.Offset = size
	}

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
// winMaxPayloadSize): copying in smaller chunks (io.Copy's default is 32KB)
// would split each buffer across far more SMB WRITE round-trips than the
// negotiated dialect allows, throttling throughput on large transfers.
const writeBufSize = 1 << 20 // 1MB

// WriteChunk appends src to the data file starting at offset.
// TUS guarantees sequential chunks, so appending is always correct.
// Only opening the file may reconnect and retry: once bytes of src are
// consumed, a retry would write the rest at the wrong place. A failure
// mid-copy returns what was written; the client then asks for the offset
// (GetUpload reads the real file size) and resumes from there.
func (u *smbUpload) WriteChunk(ctx context.Context, offset int64, src io.Reader) (int64, error) {
	binPath := u.store.smbPath(u.info.ID)
	var f *smb2.File
	err := u.store.b.withShare(func(sh *smb2.Share) error {
		var err error
		f, err = sh.OpenFile(binPath, os.O_WRONLY|os.O_APPEND, 0o644)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("smb tus: open data file for write %q: %w", binPath, err)
	}
	defer f.Close()

	buf := make([]byte, writeBufSize)
	n, err := io.CopyBuffer(f, src, buf)
	u.info.Offset += n
	return n, err
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
