package handler

// Request handler — internal user side.
//
// Routes (IP-restricted — internal network only):
//   GET  /request — render upload request creation form
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

	"github.com/your-org/ferri/internal/config"
	appMiddleware "github.com/your-org/ferri/internal/middleware"
	"github.com/your-org/ferri/internal/store"
)

// ── Request form ──────────────────────────────────────────────────────────────

// RequestPage handles GET /request.
func RequestPage(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		settings := appMiddleware.GetSettings(r)
		renderRequestPage(w, cfg, settings, "")
	}
}

// ── Request creation ──────────────────────────────────────────────────────────

// RequestCreate handles POST /request.
// Creates an upload request and renders the result page with the upload link.
func RequestCreate(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		settings := appMiddleware.GetSettings(r)

		if err := r.ParseForm(); err != nil {
			renderRequestPage(w, cfg, settings, "Invalid form data.")
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
			renderRequestPage(w, cfg, settings, "Your name is required.")
			return
		}
		if !isValidEmail(requesterEmail) {
			renderRequestPage(w, cfg, settings, "Valid email address is required.")
			return
		}

		expiryHours, err := strconv.Atoi(expiryStr)
		if err != nil || !isValidExpiryOption(expiryHours, cfg.ExpiryOptions) {
			renderRequestPage(w, cfg, settings, "Invalid expiry option.")
			return
		}

		// ── Password hashing ──────────────────────────────────────────────────

		var passwordHash string
		if password != "" {
			hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
			if err != nil {
				slog.Error("request: bcrypt hash", "error", err)
				renderRequestPage(w, cfg, settings, "Internal server error.")
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
			renderRequestPage(w, cfg, settings, "Internal server error.")
			return
		}

		uploadURL := cfg.Server.BaseURL + "/ul/" + uploadToken

		slog.Info("upload request created",
			"requester", requesterEmail,
			"expiry_hours", expiryHours,
			"has_password", passwordHash != "",
		)

		// Render result page with the upload link
		renderRequestResult(w, uploadURL, title, settings)
	}
}

// ── Template rendering placeholders ──────────────────────────────────────────

func renderRequestPage(w http.ResponseWriter, cfg *config.Config, settings *store.Settings, errMsg string) {
	renderPage(w, "request.html", struct {
		baseData
		ExpiryOptions []config.ExpiryOption
		Error         string
	}{
		baseData:      baseData{PageTitle: "Request files", Settings: settings},
		ExpiryOptions: cfg.ExpiryOptions,
		Error:         errMsg,
	})
}

func renderRequestResult(w http.ResponseWriter, uploadURL, title string, settings *store.Settings) {
	renderPage(w, "request_created.html", struct {
		baseData
		UploadURL string
		ExpiresAt string
	}{
		baseData:  baseData{PageTitle: "Upload link created", Settings: settings},
		UploadURL: uploadURL,
		ExpiresAt: "",
	})
}
