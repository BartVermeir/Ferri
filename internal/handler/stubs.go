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

import "net/http"

func stub(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Not implemented: "+name, http.StatusNotImplemented)
	}
}

// ── Send workflow ─────────────────────────────────────────────────────────────
// Implemented in send.go

// ── Download workflow ─────────────────────────────────────────────────────────
// Implemented in download.go

// ── Upload request workflow ───────────────────────────────────────────────────
// Implemented in upload.go (external side) and request.go (internal side)

// ── Admin UI ──────────────────────────────────────────────────────────────────
// Implemented in admin.go
