package handler

// Request handler — internal user side.
//
// Routes (IP-restricted — internal network only):
//   GET  /request — render upload request creation form (tab pre-selected)
//   POST /request — create upload request, return upload link
//
// Flow (architecture.md §5b step 1):
//   Internal user fills the form with title, message, optional password,
//   expiry, and their own email for notification.
//   POST /request creates the upload_request row and returns the upload link
//   (/ul/:token) which the internal user sends to the external party.

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/BartVermeir/Ferri/internal/config"
	appMiddleware "github.com/BartVermeir/Ferri/internal/middleware"
	"github.com/BartVermeir/Ferri/internal/store"
)

// ── Request form ──────────────────────────────────────────────────────────────

// RequestPage handles GET /request.
// Renders the combined send/request page with the request tab pre-selected.
func RequestPage(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		settings := appMiddleware.GetSettings(r)
		renderHomePage(w, cfg, settings, "request", "")
	}
}

// ── Request creation ──────────────────────────────────────────────────────────

// RequestCreate handles POST /request.
// Creates an upload request and renders the result page with the upload link.
func RequestCreate(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		settings := appMiddleware.GetSettings(r)

		r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
		if err := r.ParseForm(); err != nil {
			renderHomePage(w, cfg, settings, "request", "Invalid form data.")
			return
		}

		// ── Input validation ──────────────────────────────────────────────────

		requesterName := strings.TrimSpace(r.FormValue("requester_name"))
		requesterEmail := strings.TrimSpace(r.FormValue("requester_email"))
		title := strings.TrimSpace(r.FormValue("title"))
		message := strings.TrimSpace(r.FormValue("message"))
		password := r.FormValue("password")
		expiryStr := r.FormValue("expiry_hours")

		if requesterName == "" {
			renderHomePage(w, cfg, settings, "request", "Your name is required.")
			return
		}
		if len(requesterName) > maxNameLen {
			renderHomePage(w, cfg, settings, "request", "Your name is too long.")
			return
		}
		if !isValidEmail(requesterEmail) {
			renderHomePage(w, cfg, settings, "request", "Valid email address is required.")
			return
		}
		if len(title) > maxTitleLen {
			renderHomePage(w, cfg, settings, "request", "Title is too long.")
			return
		}
		if len(message) > maxMessageLen {
			renderHomePage(w, cfg, settings, "request", "Message is too long.")
			return
		}

		expiryHours, err := strconv.Atoi(expiryStr)
		if err != nil || !isValidExpiryOption(expiryHours, cfg.ExpiryOptions) {
			renderHomePage(w, cfg, settings, "request", "Invalid expiry option.")
			return
		}

		// ── Password hashing ──────────────────────────────────────────────────

		var passwordHash string
		if password != "" {
			hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
			if err != nil {
				slog.Error("request: bcrypt hash", "error", err)
				renderHomePage(w, cfg, settings, "request", "Internal server error.")
				return
			}
			passwordHash = string(hash)
		}

		// ── Create upload request ─────────────────────────────────────────────

		expiresAt := time.Now().Add(time.Duration(expiryHours) * time.Hour)

		input := store.CreateRequestInput{
			Title:          title,
			Message:        message,
			RequesterName:  requesterName,
			RequesterEmail: requesterEmail,
			PasswordHash:   passwordHash,
			ExpiresAt:      expiresAt,
		}

		_, uploadToken, err := stores.Requests.Create(input)
		if err != nil {
			slog.Error("request: create", "error", err)
			renderHomePage(w, cfg, settings, "request", "Internal server error.")
			return
		}
		// The view token is generated inside Create; read it back rather than
		// widen Create's signature for this one caller.
		created, err := stores.Requests.GetByUploadToken(uploadToken)
		if err != nil || created == nil {
			slog.Error("request: read back created request", "error", err)
			renderHomePage(w, cfg, settings, "request", "Internal server error.")
			return
		}

		uploadURL := cfg.Server.BaseURL + "/ul/" + uploadToken
		viewURL := cfg.Server.BaseURL + "/ul/" + created.ViewPathToken() + "/files"

		slog.Info("upload request created",
			"requester", requesterEmail,
			"expiry_hours", expiryHours,
			"has_password", passwordHash != "",
		)

		// Render result page with the upload link
		renderRequestResult(w, uploadURL, viewURL, settings)
	}
}

// ── Template rendering ────────────────────────────────────────────────────────

// renderRequestResult shows both links: the upload link for the external
// party, and the requester's own view link. They differ on purpose (audit M1).
func renderRequestResult(w http.ResponseWriter, uploadURL, viewURL string, settings *store.Settings) {
	renderPage(w, "request_created.html", struct {
		baseData
		UploadURL string
		ViewURL   string
	}{
		baseData:  baseData{PageTitle: "Upload link created", Settings: settings},
		UploadURL: uploadURL,
		ViewURL:   viewURL,
	})
}
