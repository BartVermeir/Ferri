package handler

// This file contains stub implementations of all handlers.
// Each stub returns HTTP 501 Not Implemented.
// Replace each stub with a full implementation as development progresses.
//
// Implementation order (recommended):
//   1. download.go  — critical path for recipients
//   2. send.go      — core upload workflow
//   3. tus          — resumable upload handling (see internal/tus/)
//   4. admin.go     — admin UI
//   5. upload.go    — upload request workflow
//   6. request.go   — upload request creation

import (
	"net/http"

	"github.com/your-org/ferri/internal/config"
	"github.com/your-org/ferri/internal/store"
)

func stub(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Not implemented: "+name, http.StatusNotImplemented)
	}
}

// ── Send workflow ─────────────────────────────────────────────────────────────

func SendPage(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return stub("SendPage")
}

func SendCreate(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return stub("SendCreate")
}

// ── Download workflow ─────────────────────────────────────────────────────────
// Implemented in download.go

// ── Upload request workflow ───────────────────────────────────────────────────

func RequestPage(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return stub("RequestPage")
}

func RequestCreate(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return stub("RequestCreate")
}

func UploadPage(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return stub("UploadPage")
}

func UploadPassword(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return stub("UploadPassword")
}

func UploadComplete(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return stub("UploadComplete")
}

// ── Admin UI ──────────────────────────────────────────────────────────────────

func AdminLogin(cfg *config.Config) http.HandlerFunc {
	return stub("AdminLogin")
}

func AdminLoginPost(cfg *config.Config) http.HandlerFunc {
	return stub("AdminLoginPost")
}

func AdminLogout() http.HandlerFunc {
	return stub("AdminLogout")
}

func AdminDashboard(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return stub("AdminDashboard")
}

func AdminTransfers(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return stub("AdminTransfers")
}

func AdminTransferDelete(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return stub("AdminTransferDelete")
}

func AdminMail(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return stub("AdminMail")
}

func AdminMailRetry(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return stub("AdminMailRetry")
}

func AdminMailDelete(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return stub("AdminMailDelete")
}

func AdminSettings(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return stub("AdminSettings")
}

func AdminSettingsSave(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return stub("AdminSettingsSave")
}
