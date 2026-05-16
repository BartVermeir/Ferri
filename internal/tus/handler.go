package tus

// TUS handler for Ferri.
//
// This file wires tusd into the application with:
//   - Token validation in PreUploadCreateCallback (before any bytes are written)
//   - Size limit enforcement via Upload-Length header check
//   - tus_last_activity_at update on every PATCH (for stalled upload detection)
//   - Atomic transfer activation via TryActivate on upload completion
//   - Mail queue inserts on activation (not direct SMTP sends)
//
// See architecture.md §4 (tus component) for the full specification.
//
// TODO: implement this file using github.com/tus/tusd/v2

import (
	"net/http"

	"github.com/your-org/ferri/internal/config"
	"github.com/your-org/ferri/internal/store"
)

// Handler is the HTTP handler for TUS uploads.
type Handler struct {
	cfg    *config.Config
	stores *store.Stores
}

// NewHandler creates a new TUS handler with token validation and DB callbacks.
func NewHandler(cfg *config.Config, stores *store.Stores) (*Handler, error) {
	return &Handler{cfg: cfg, stores: stores}, nil
}

// ServeHTTP satisfies http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "TUS handler not yet implemented", http.StatusNotImplemented)
}
