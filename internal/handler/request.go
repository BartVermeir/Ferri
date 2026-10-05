package handler

// Request handler — internal user side.
//
// Routes (IP-restricted — internal network only):
//   GET  /request — render upload request creation form (tab pre-selected)
//   POST /request — create one or more upload requests, show their links
//
// Flow:
//   Internal user fills the form with title, message, optional password,
//   expiry, and their own email for notification.
//   POST /request creates the upload_request row and returns the upload link
//   (/ul/:token) which the internal user sends to the external party.

import (
	"errors"
	"fmt"
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
// Creates one or more upload requests (link_count, same settings for each)
// and renders the result page with their links.
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

		// No link_count in the form: one link.
		count := 1
		if v := strings.TrimSpace(r.FormValue("link_count")); v != "" {
			count, err = strconv.Atoi(v)
			if err != nil || count < 1 || count > maxRequestLinks {
				renderHomePage(w, cfg, settings, "request", fmt.Sprintf("Number of links must be between 1 and %d.", maxRequestLinks))
				return
			}
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

		// ── Create upload requests ────────────────────────────────────────────

		// Each link is an ordinary, separate request: its own tokens, mails
		// and manage page. They share the settings and the expiry moment.
		expiresAt := time.Now().Add(time.Duration(expiryHours) * time.Hour)

		var links []requestLinks
		for i := 1; i <= count; i++ {
			linkTitle := title
			if count > 1 {
				linkTitle = strings.TrimSpace(fmt.Sprintf("%s #%d", title, i))
			}
			l, err := createRequest(cfg, stores, store.CreateRequestInput{
				Title:          linkTitle,
				Message:        message,
				RequesterName:  requesterName,
				RequesterEmail: requesterEmail,
				PasswordHash:   passwordHash,
				ExpiresAt:      expiresAt,
			})
			if err != nil {
				slog.Error("request: create", "link", i, "of", count, "error", err)
				break
			}
			links = append(links, l)
		}
		if len(links) == 0 {
			renderHomePage(w, cfg, settings, "request", "Internal server error.")
			return
		}

		slog.Info("upload request created",
			"links", len(links),
			"expiry_hours", expiryHours,
			"has_password", passwordHash != "",
		)

		// The ones that were created work and are listed, so the page still
		// shows them; it says how many are missing.
		var errMsg string
		if len(links) < count {
			errMsg = fmt.Sprintf("Only %d of %d links could be created. Create the others as a new request.", len(links), count)
		}
		renderRequestResult(w, links, errMsg, settings)
	}
}

// requestLinks are the three links of one created request.
type requestLinks struct {
	Title     string
	UploadURL string // for the external party
	ViewURL   string // the requester's own view of what came in
	ManageURL string
}

func createRequest(cfg *config.Config, stores *store.Stores, input store.CreateRequestInput) (requestLinks, error) {
	_, uploadToken, err := stores.Requests.Create(input)
	if err != nil {
		return requestLinks{}, err
	}
	// Create generates the view and manage tokens; they are read back from
	// the stored request.
	created, err := stores.Requests.GetByUploadToken(uploadToken)
	if err != nil {
		return requestLinks{}, fmt.Errorf("read back created request: %w", err)
	}
	if created == nil {
		return requestLinks{}, errors.New("created request not found")
	}
	return requestLinks{
		Title:     input.Title,
		UploadURL: cfg.Server.BaseURL + "/ul/" + uploadToken,
		ViewURL:   cfg.Server.BaseURL + "/ul/" + created.ViewPathToken() + "/files",
		ManageURL: cfg.Server.BaseURL + "/manage/" + created.ManageToken.String,
	}, nil
}

// ── Template rendering ────────────────────────────────────────────────────────

// renderRequestResult shows the links of each created request: the upload
// link for the external party, and the requester's own view link and manage
// link. The upload and view link use different tokens.
func renderRequestResult(w http.ResponseWriter, links []requestLinks, errMsg string, settings *store.Settings) {
	title := "Upload link created"
	if len(links) > 1 {
		title = fmt.Sprintf("%d upload links created", len(links))
	}
	renderPage(w, "request_created.html", struct {
		baseData
		Links []requestLinks
		Error string
	}{
		baseData: baseData{PageTitle: title, Settings: settings},
		Links:    links,
		Error:    errMsg,
	})
}
