package middleware

import (
	"context"
	"net/http"

	"github.com/your-org/ferri/internal/store"
)

type contextKey string

const settingsKey contextKey = "settings"

// InjectSettings returns middleware that loads runtime settings into the request context.
func InjectSettings(s *store.SettingsStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			settings := s.Get()
			ctx := context.WithValue(r.Context(), settingsKey, settings)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// GetSettings retrieves settings from the request context.
func GetSettings(r *http.Request) *store.Settings {
	if s, ok := r.Context().Value(settingsKey).(*store.Settings); ok {
		return s
	}
	return &store.Settings{}
}
