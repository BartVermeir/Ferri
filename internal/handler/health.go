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
// Returns HTTP 200 with {"status":"ok"} when the database is reachable, and
// HTTP 503 otherwise so the Docker healthcheck and external monitoring see a
// wedged database as unhealthy rather than "ok".
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

// Static returns a handler that serves embedded static files from web/static/.
// Files are embedded at compile time via the static package — no runtime
// filesystem dependency. The binary is fully self-contained.
func Static() http.Handler {
	sub, err := fs.Sub(static.FS, "files")
	if err != nil {
		panic("static: " + err.Error())
	}
	return http.FileServer(http.FS(sub))
}

// LogoFileServer serves the uploaded branding logo from dir on the local
// filesystem. Logos are deliberately kept on local disk even when the file
// storage backend is SMB (see AdminLogoUpload) — they are small, public, and
// not part of user data. Unlike a bare http.FileServer this never renders a
// directory listing: any request that resolves to a directory is a 404.
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
