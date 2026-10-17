package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBodyLimit(t *testing.T) {
	var read int
	h := BodyLimit(10)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		read = len(b)
	}))
	for path, want := range map[string]int{"/dl/x": 10, "/tus/abc": 100, "/tus": 100} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path, strings.NewReader(strings.Repeat("a", 100))))
		if read != want {
			t.Errorf("%s: read %d bytes, want %d", path, read, want)
		}
	}
}
