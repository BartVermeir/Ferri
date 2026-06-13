package handler

// stub returns a 501 Not Implemented handler.
// Used as a placeholder during development; no stubs remain active in production.
import "net/http"

func stub(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Not implemented: "+name, http.StatusNotImplemented)
	}
}
