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

// smbTUSStore is a tusd.DataStore backed by an SMB share.
type smbTUSStore struct {
	share    *smb2.Share
	basePath string // path prefix on the share, e.g. "" or "ferri"
}

func newSMBTUSStore(share *smb2.Share, basePath string) *smbTUSStore {
	return &smbTUSStore{share: share, basePath: basePath}
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
	f, err := s.share.OpenFile(binPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("smb tus: create data file %q: %w", binPath, err)
	}
	f.Close()

	// Write .info file
	upload := &smbUpload{store: s, info: info}
	if err := upload.writeInfo(); err != nil {
		return nil, fmt.Errorf("smb tus: write info file: %w", err)
	}

	return upload, nil
}

// GetUpload retrieves an existing TUS upload from the SMB share.
func (s *smbTUSStore) GetUpload(ctx context.Context, id string) (tusd.Upload, error) {
	infoPath := s.smbPath(id + ".info")
	f, err := s.share.Open(infoPath)
	if err != nil {
		return nil, tusd.ErrNotFound
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("smb tus: read info file %q: %w", infoPath, err)
	}

	var info tusd.FileInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("smb tus: parse info file %q: %w", infoPath, err)
	}

	// Sync offset from actual data file size (mirrors filestore behaviour).
	binPath := s.smbPath(id)
	fi, err := s.share.Stat(binPath)
	if err == nil {
		info.Offset = fi.Size()
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

// WriteChunk appends src to the data file starting at offset.
// TUS guarantees sequential chunks, so appending is always correct.
func (u *smbUpload) WriteChunk(ctx context.Context, offset int64, src io.Reader) (int64, error) {
	binPath := u.store.smbPath(u.info.ID)
	f, err := u.store.share.OpenFile(binPath, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, fmt.Errorf("smb tus: open data file for write %q: %w", binPath, err)
	}
	defer f.Close()

	n, err := io.Copy(f, src)
	u.info.Offset += n
	return n, err
}

func (u *smbUpload) GetReader(ctx context.Context) (io.ReadCloser, error) {
	binPath := u.store.smbPath(u.info.ID)
	f, err := u.store.share.Open(binPath)
	if err != nil {
		return nil, fmt.Errorf("smb tus: open data file for read %q: %w", binPath, err)
	}
	return f, nil
}

// FinishUpload updates the .info file with the final offset.
func (u *smbUpload) FinishUpload(ctx context.Context) error {
	return u.writeInfo()
}

func (u *smbUpload) writeInfo() error {
	infoPath := u.store.smbPath(u.info.ID + ".info")
	data, err := json.Marshal(u.info)
	if err != nil {
		return fmt.Errorf("marshal info: %w", err)
	}
	f, err := u.store.share.OpenFile(infoPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open info file %q: %w", infoPath, err)
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}
