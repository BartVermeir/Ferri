package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"net/http"
	"time"

	"github.com/BartVermeir/Ferri/static"
)

// Health returns a handler for GET /health.
// Returns HTTP 200 with {"status":"ok"} when the database answers a ping
// within 2 seconds, and HTTP 503 otherwise. The Docker healthcheck uses it.
func Health(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "error", "detail": "database unreachable"})
			return
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}

// Static returns a handler that serves embedded static files from static/files/.
// Files are embedded at compile time via the static package.
func Static() http.Handler {
	sub, err := fs.Sub(static.FS, "files")
	if err != nil {
		panic("static: " + err.Error())
	}
	return http.FileServer(http.FS(sub))
}

// LogoFileServer serves the uploaded branding logo from dir on the local
// filesystem, also when the file storage backend is SMB (see AdminLogoUpload).
// It never renders a directory listing: any request that resolves to a
// directory is a 404.
func LogoFileServer(dir string) http.Handler {
	return http.FileServer(noListingFS{http.Dir(dir)})
}

type noListingFS struct{ inner http.FileSystem }

func (n noListingFS) Open(name string) (http.File, error) {
	f, err := n.inner.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if info.IsDir() {
		f.Close()
		return nil, fs.ErrNotExist
	}
	return f, nil
}
