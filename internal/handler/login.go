package handler

// Sign-in for colleagues outside the internal network (remote_login in
// config.yaml). Public routes; all of them answer 404 while it is off.
//
//   POST /login          — mail a sign-in link to a work address
//   GET  /login/{token}  — page with a "Sign in" button; the link is not used yet
//   POST /login/{token}  — use the link, set the sign-in cookie, go to /
//   POST /logout         — remove the sign-in cookie
//
// The GET does not use the link because mail scanners open links in mails
// before the reader does.

import (
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/BartVermeir/Ferri/internal/config"
	"github.com/BartVermeir/Ferri/internal/mail"
	appMiddleware "github.com/BartVermeir/Ferri/internal/middleware"
	"github.com/BartVermeir/Ferri/internal/store"
)

const (
	// loginLinkTTL is how long a mailed sign-in link works.
	loginLinkTTL = 15 * time.Minute
	// loginLinksPerAddress links at most per address within loginLinkTTL;
	// more requests get the same answer but no mail.
	loginLinksPerAddress = 3
)

// MailKicker starts sending queued mail right away (jobs.Scheduler).
type MailKicker interface {
	KickMail()
}

// loginPageData is the template data for login.html.
type loginPageData struct {
	baseData
	// State: "sent" (check your mail), "confirm" (button to sign in),
	// "invalid" (link used or expired; form to ask a new one) or "ask"
	// (form with an error).
	State   string
	Email   string
	Error   string
	Minutes int
}

func renderLogin(w http.ResponseWriter, r *http.Request, status int, data loginPageData) {
	data.baseData = baseData{PageTitle: "Sign in", Settings: appMiddleware.GetSettings(r)}
	data.Minutes = int(loginLinkTTL.Minutes())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	renderPage(w, "login.html", data)
}

// LoginRequest handles POST /login: it mails a sign-in link to an address on
// one of remote_login.domains.
func LoginRequest(cfg *config.Config, stores *store.Stores, kicker MailKicker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !cfg.RemoteLoginEnabled() {
			http.NotFound(w, r)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
		if err := r.ParseForm(); err != nil {
			renderLogin(w, r, http.StatusBadRequest, loginPageData{State: "ask", Error: "Could not read the form. Please try again."})
			return
		}
		email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
		if !isValidEmail(email) {
			renderLogin(w, r, http.StatusBadRequest, loginPageData{State: "ask", Email: email, Error: "Enter a valid email address."})
			return
		}
		if !cfg.RemoteLoginAllows(email) {
			renderLogin(w, r, http.StatusBadRequest, loginPageData{State: "ask", Email: email, Error: "This address cannot sign in. Use your work email address."})
			return
		}

		tok, err := stores.Logins.Create(email, loginLinkTTL, loginLinkTTL, loginLinksPerAddress)
		if err != nil {
			slog.Error("login: create link", "error", err)
			renderLogin(w, r, http.StatusInternalServerError, loginPageData{State: "ask", Email: email, Error: "Something went wrong on our side. Please try again later."})
			return
		}
		if tok == "" {
			slog.Warn("login: link limit reached for an address")
		} else {
			settings := appMiddleware.GetSettings(r)
			m := loginMail{
				URL:     cfg.Server.BaseURL + "/login/" + tok,
				Company: mail.CompanyName(settings),
				Host:    siteHostOf(cfg.Server.BaseURL),
				Minutes: int(loginLinkTTL.Minutes()),
			}
			subject := "Your sign-in link for " + m.Company
			if err := stores.Mail.Enqueue(nil, store.MailAbout{}, email, subject,
				mail.Wrap(settings, cfg.Server.BaseURL, m.preheader(), buildLoginHTML(m, settings)), buildLoginText(m)); err != nil {
				slog.Error("login: enqueue mail", "error", err)
				renderLogin(w, r, http.StatusInternalServerError, loginPageData{State: "ask", Email: email, Error: "Something went wrong on our side. Please try again later."})
				return
			}
			if kicker != nil {
				kicker.KickMail()
			}
			slog.Info("login: link mailed")
		}
		renderLogin(w, r, http.StatusOK, loginPageData{State: "sent", Email: email})
	}
}

// LoginPage handles GET /login/{token}: a valid link shows a button that
// signs in; the link itself stays unused until then.
func LoginPage(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !cfg.RemoteLoginEnabled() {
			http.NotFound(w, r)
			return
		}
		email, ok, err := stores.Logins.Peek(chi.URLParam(r, "token"))
		if err != nil {
			slog.Error("login: read link", "error", err)
		}
		if !ok {
			renderLogin(w, r, http.StatusNotFound, loginPageData{State: "invalid"})
			return
		}
		renderLogin(w, r, http.StatusOK, loginPageData{State: "confirm", Email: email})
	}
}

// LoginConfirm handles POST /login/{token}: it uses the link, sets the
// sign-in cookie and goes to the send page.
func LoginConfirm(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !cfg.RemoteLoginEnabled() {
			http.NotFound(w, r)
			return
		}
		email, ok, err := stores.Logins.Consume(chi.URLParam(r, "token"))
		if err != nil {
			slog.Error("login: use link", "error", err)
		}
		if !ok || !cfg.RemoteLoginAllows(email) {
			renderLogin(w, r, http.StatusNotFound, loginPageData{State: "invalid"})
			return
		}
		appMiddleware.SetRemoteCookie(w, cfg, email)
		slog.Info("login: signed in", "hours", cfg.RemoteLogin.SessionHours)
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

// Logout handles POST /logout.
func Logout(cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !cfg.RemoteLoginEnabled() {
			http.NotFound(w, r)
			return
		}
		appMiddleware.ClearRemoteCookie(w, cfg)
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

// ── Mail ──────────────────────────────────────────────────────────────────────

type loginMail struct {
	URL     string
	Company string
	Host    string
	Minutes int
}

func (m loginMail) preheader() string {
	return fmt.Sprintf("Your sign-in link, valid for %d minutes.", m.Minutes)
}

func (m loginMail) intro() string {
	return fmt.Sprintf("Use the button below to sign in to %s file transfer. The link works once, within %d minutes.", m.Company, m.Minutes)
}

func (m loginMail) note() string {
	return fmt.Sprintf("You get this mail because a sign-in link was asked for this address on %s. Not you? Then ignore this mail: without the link, nobody signs in.", m.Host)
}

func buildLoginHTML(m loginMail, settings *store.Settings) string {
	return mail.HelloHTML("") +
		`<p style="margin:0 0 20px;">` + html.EscapeString(m.intro()) + `</p>` +
		mail.ButtonHTML(m.URL, "Sign in", settings) +
		mail.NoteHTML(m.note())
}

func buildLoginText(m loginMail) string {
	return mail.HelloText("") +
		m.intro() + "\n\n" +
		"Sign in: " + m.URL + "\n\n" +
		"--\n" + m.note() + "\n"
}

// siteHostOf returns the host of baseURL ("send.example.com").
func siteHostOf(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	return u.Host
}
