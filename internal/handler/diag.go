package handler

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/BartVermeir/Ferri/internal/storage"
)

// AdminDiag lists the uploads the upload diagnostics measured, newest first,
// as plain text.
func AdminDiag(mgr *storage.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !mgr.DiagEnabled() {
			http.Error(w, "Upload diagnostics are off (storage.diag_upload_hashes in config.yaml).", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "Block size %d bytes. Blocks per upload: /admin/diag/<tus_id>\n", storage.DiagBlockSize)
		fmt.Fprintf(w, "not measured = no received hash (resumed mid-block), differ = stored block not what was received\n\n")
		for _, u := range mgr.DiagUploads() {
			fmt.Fprintf(w, "%s  %s  %s\n  size %d, stored %d, blocks %d, not measured %d, differ %d, %s\n\n",
				u.Started.In(displayLocation).Format("2006-01-02 15:04:05"), u.ID, "file "+u.FileID,
				u.Size, u.StoredSize, u.Blocks, u.NotMeasured, u.Differ, u.State)
		}
	}
}

// AdminDiagUpload gives one upload's blocks, a line each:
// "<offset> <received sha256> <stored sha256>", "-" when unknown.
func AdminDiagUpload(mgr *storage.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "tusID")
		if !mgr.DiagEnabled() || !isTUSUploadID(id) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="diag-`+id+`.txt"`)
		if !mgr.WriteDiagBlocks(id, w) {
			w.Header().Del("Content-Disposition")
			http.NotFound(w, r)
		}
	}
}
