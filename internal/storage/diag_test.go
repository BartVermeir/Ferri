package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tusd "github.com/tus/tusd/v2/pkg/handler"
)

func sumHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// diagManager: a Manager on a local backend with diagnostics on, blocks of 4
// bytes, and no background read-back (tests call verifyOne).
func diagManager(t *testing.T) (*Manager, string) {
	t.Helper()
	root := t.TempDir()
	m := NewManager(NewLocalBackend(root))
	m.diag = newDiagRecorder(4, m.Open)
	return m, root
}

// ticked: the metadata of a transfer upload sent with "Check upload integrity".
func ticked() tusd.MetaData {
	return tusd.MetaData{"ferri_file_id": "f1", "transfer_id": "t1", "ferri_diag": "1"}
}

// upload sends data through the Manager's TUS store in the given chunks.
func upload(t *testing.T, m *Manager, data []byte, chunks ...int) string {
	t.Helper()
	ctx := context.Background()
	store := m.TUSDataStore()
	up, err := store.NewUpload(ctx, tusd.FileInfo{Size: int64(len(data)), MetaData: ticked()})
	if err != nil {
		t.Fatal(err)
	}
	info, _ := up.GetInfo(ctx)
	off := 0
	for _, c := range chunks {
		u, err := store.GetUpload(ctx, info.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := u.WriteChunk(ctx, int64(off), bytes.NewReader(data[off:off+c])); err != nil {
			t.Fatal(err)
		}
		off += c
	}
	u, _ := store.GetUpload(ctx, info.ID)
	if err := u.FinishUpload(ctx); err != nil {
		t.Fatal(err)
	}
	<-m.diag.verify // queued by FinishUpload
	m.diag.verifyOne(info.ID)
	return info.ID
}

func blockLines(t *testing.T, m *Manager, id string) []string {
	t.Helper()
	var buf bytes.Buffer
	if !m.WriteDiagBlocks(id, &buf) {
		t.Fatal("upload not known")
	}
	return strings.Split(strings.TrimSpace(buf.String()), "\n")
}

func TestDiag_ReceivedMatchesStored(t *testing.T) {
	m, _ := diagManager(t)
	data := []byte("0123456789")   // blocks: 0123 4567 89
	id := upload(t, m, data, 6, 4) // the second chunk continues block 1

	want := []string{
		"0 " + sumHex(data[0:4]) + " " + sumHex(data[0:4]),
		"4 " + sumHex(data[4:8]) + " " + sumHex(data[4:8]),
		"8 " + sumHex(data[8:10]) + " " + sumHex(data[8:10]),
	}
	if got := blockLines(t, m, id); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("blocks:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	u := m.DiagUploads()[0]
	if u.Differ != 0 || u.NotMeasured != 0 || u.State != DiagStateVerified || u.FileID != "f1" {
		t.Errorf("summary = %+v", u)
	}
}

func TestDiag_StoredBlockChangedIsReported(t *testing.T) {
	m, root := diagManager(t)
	data := []byte("0123456789")
	ctx := context.Background()
	store := m.TUSDataStore()
	up, _ := store.NewUpload(ctx, tusd.FileInfo{Size: 10, MetaData: ticked()})
	info, _ := up.GetInfo(ctx)
	if _, err := up.WriteChunk(ctx, 0, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	// Byte 5 changed on storage.
	p := filepath.Join(root, info.ID)
	stored, _ := os.ReadFile(p)
	stored[5] = 'X'
	if err := os.WriteFile(p, stored, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := up.FinishUpload(ctx); err != nil {
		t.Fatal(err)
	}
	<-m.diag.verify
	m.diag.verifyOne(info.ID)

	if u := m.DiagUploads()[0]; u.Differ != 1 {
		t.Fatalf("differ = %d, want 1", u.Differ)
	}
	if got := blockLines(t, m, info.ID)[1]; got != "4 "+sumHex(data[4:8])+" "+sumHex(stored[4:8]) {
		t.Errorf("block 1 = %q", got)
	}
}

// A chunk that fails halfway keeps only the blocks it wrote; the retry starts
// mid-block, so that block has no received hash, the ones after it do.
func TestDiag_FailedChunkAndUnalignedResume(t *testing.T) {
	d := newDiagRecorder(4, nil)
	data := []byte("0123456789")

	r := d.begin("id", "", 10, 0, bytes.NewReader(data[0:8]))
	io.Copy(io.Discard, r) // read 8 bytes (blocks 0 and 1 complete)
	r.done(5, errors.New("write failed"))

	r = d.begin("id", "", 10, 5, bytes.NewReader(data[5:10]))
	io.Copy(io.Discard, r)
	r.done(5, nil)
	d.finish("id", "", 10)

	got := d.uploads["id"].received
	if len(got) != 3 || !got[0].ok || got[1].ok || !got[2].ok {
		t.Fatalf("received = %v", got)
	}
	if got[0].String() != sumHex(data[0:4]) || got[2].String() != sumHex(data[8:10]) {
		t.Errorf("hashes = %v %v", got[0], got[2])
	}
}

// Only a ticked transfer upload is measured: not one without the tick, and
// not a request upload (the request upload page is public).
func TestDiag_OnlyTickedTransferUploads(t *testing.T) {
	m, _ := diagManager(t)
	ctx := context.Background()
	store := m.TUSDataStore()
	for _, meta := range []tusd.MetaData{
		{"transfer_id": "t1"},
		{"upload_request_token": "r1", "ferri_diag": "1"},
	} {
		up, err := store.NewUpload(ctx, tusd.FileInfo{Size: 4, MetaData: meta})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := up.WriteChunk(ctx, 0, strings.NewReader("abcd")); err != nil {
			t.Fatal(err)
		}
		if err := up.FinishUpload(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := m.DiagUploads(); len(got) != 0 {
		t.Errorf("measured %d uploads, want 0", len(got))
	}
}

func TestDiag_OffDoesNothing(t *testing.T) {
	m := NewManager(NewLocalBackend(t.TempDir()))
	if m.DiagEnabled() || m.DiagUploads() != nil || m.WriteDiagBlocks("x", io.Discard) {
		t.Fatal("diagnostics should be off")
	}
}

// Removing an upload's data drops its integrity record.
func TestDiag_RemoveUploadForgetsRecord(t *testing.T) {
	m, _ := diagManager(t)
	id := upload(t, m, []byte("0123456789"), 10)
	if _, ok := m.DiagUploadByID(id); !ok {
		t.Fatal("upload not known before removal")
	}
	if err := m.RemoveUpload("", id); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.DiagUploadByID(id); ok {
		t.Error("record still known after RemoveUpload")
	}
	if n := len(m.DiagUploads()); n != 0 {
		t.Errorf("DiagUploads lists %d uploads, want 0", n)
	}
}
