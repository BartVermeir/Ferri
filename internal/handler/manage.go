package handler

// Manage page — for the sender of a transfer and the requester of an upload
// request (DEC-043).
//
// Routes (IP-restricted — internal network only, like the send page):
//   GET  /manage/{token}         — status, files, and per recipient what was downloaded
//   POST /manage/{token}/extend  — move the expiry later, to one of expiry_options from now
//   POST /manage/{token}/delete  — delete at once, same path as the admin delete
//
// One manage token per transfer or request (migration 006), handed out in the
// sender's confirmation mail, on the screen after an upload, on the "upload
// link created" page and in the requester's mails. The token alone opens the
// page, so it only answers on the internal network.

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/BartVermeir/Ferri/internal/config"
	appMiddleware "github.com/BartVermeir/Ferri/internal/middleware"
	"github.com/BartVermeir/Ferri/internal/storage"
	"github.com/BartVermeir/Ferri/internal/store"
)

// managed is what a manage token points at: a live transfer or a live
// request, never both.
type managed struct {
	Transfer *store.Transfer
	Request  *store.UploadRequest
}

// minExtension: an extend must add at least this much. Without it "1 day"
// was offered for a transfer created a moment ago for one day, and "extended"
// it by a few milliseconds.
const minExtension = time.Hour

func (m managed) found() bool { return m.Transfer != nil || m.Request != nil }

func (m managed) expiresAt() time.Time {
	if m.Transfer != nil {
		return m.Transfer.ExpiresAt
	}
	return m.Request.ExpiresAt
}

// lookupManaged finds the live transfer or request behind a manage token.
func lookupManaged(stores *store.Stores, tok string) (managed, error) {
	t, err := stores.Transfers.GetLiveByManageToken(tok)
	if err != nil || t != nil {
		return managed{Transfer: t}, err
	}
	r, err := stores.Requests.GetLiveByManageToken(tok)
	return managed{Request: r}, err
}

// ── Page data ─────────────────────────────────────────────────────────────────

type manageFile struct {
	Name string
	Size int64
}

// manageRecipient is one line of "who downloaded what".
type manageRecipient struct {
	Label  string
	Status string    // "Not downloaded yet", "3 of 5 files", "All 5 files", "Downloaded"
	Last   time.Time // last download; zero = none
}

type managePageData struct {
	baseData
	Token      string
	IsTransfer bool
	Title      string
	Status     string // shown as a badge
	Badge      string // badge class suffix: pending, active
	CreatedAt  time.Time
	ExpiresAt  time.Time
	Files      []manageFile
	TotalBytes int64
	Recipients []manageRecipient // transfers only
	LinkOnly   bool
	UploadURL  string // requests only
	ViewURL    string // requests only
	// Options are the expiry options that would make it last longer than
	// now; empty when it already runs for the longest option.
	Options  []config.ExpiryOption
	Extended bool
	Error    string
}

func buildManagePage(cfg *config.Config, stores *store.Stores, settings *store.Settings, tok string, m managed) (managePageData, error) {
	d := managePageData{
		baseData:  baseData{Settings: settings},
		Token:     tok,
		ExpiresAt: m.expiresAt(),
	}
	now := time.Now()
	for _, o := range cfg.ExpiryOptions {
		if now.Add(time.Duration(o.Hours) * time.Hour).After(d.ExpiresAt.Add(minExtension)) {
			d.Options = append(d.Options, o)
		}
	}

	if t := m.Transfer; t != nil {
		d.IsTransfer = true
		d.PageTitle = "Manage transfer"
		d.Title = t.Title
		d.CreatedAt = t.CreatedAt
		d.LinkOnly = !t.NotifyRecipients
		d.Status, d.Badge = "Live", "active"
		if t.Status == "pending" {
			d.Status, d.Badge = "Uploading", "pending"
		}
		files, err := stores.Transfers.GetFilesByTransferID(t.ID)
		if err != nil {
			return d, err
		}
		complete := map[string]bool{}
		for _, f := range files {
			if f.Status != "complete" {
				continue
			}
			complete[f.ID] = true
			d.Files = append(d.Files, manageFile{Name: f.OriginalName, Size: f.SizeBytes})
			d.TotalBytes += f.SizeBytes
		}
		history, err := stores.Downloads.GetHistoryForTransfer(t.ID)
		if err != nil {
			return d, err
		}
		for _, h := range history {
			d.Recipients = append(d.Recipients, manageRecipientLine(h, t, complete))
		}
		return d, nil
	}

	r := m.Request
	d.PageTitle = "Manage upload request"
	d.Title = r.Title
	d.CreatedAt = r.CreatedAt
	d.Status, d.Badge = "Waiting for uploads", "pending"
	if r.Status == "completed" {
		d.Status, d.Badge = "Upload complete", "active"
	}
	d.UploadURL = cfg.Server.BaseURL + "/ul/" + r.UploadToken
	d.ViewURL = cfg.Server.BaseURL + "/ul/" + r.ViewPathToken() + "/files"
	files, err := stores.Requests.GetFiles(r.ID)
	if err != nil {
		return d, err
	}
	for _, f := range completeRequestFiles(files) {
		d.Files = append(d.Files, manageFile{Name: f.OriginalName, Size: f.SizeBytes})
		d.TotalBytes += f.SizeBytes
	}
	return d, nil
}

// manageRecipientLine sums up one recipient's downloads: how many of the
// transfer's files, and when last. Same labels as the expiry summary.
func manageRecipientLine(h store.RecipientHistory, t *store.Transfer, complete map[string]bool) manageRecipient {
	line := manageRecipient{Label: h.Email, Status: "Not downloaded yet"}
	switch {
	case h.IsSender:
		line.Label = "You (your own link)"
	case !t.NotifyRecipients:
		line.Label = "Your shared link"
	}
	if len(h.Events) == 0 {
		return line
	}
	got := map[string]bool{}
	for _, ev := range h.Events {
		if ev.FileID.Valid && complete[ev.FileID.String] {
			got[ev.FileID.String] = true
		}
		if at := time.Unix(ev.DownloadedAt, 0); at.After(line.Last) {
			line.Last = at
		}
	}
	switch total := len(complete); {
	case total <= 1:
		line.Status = "Downloaded"
	case len(got) == total:
		line.Status = "All " + strconv.Itoa(total) + " files"
	default:
		line.Status = strconv.Itoa(len(got)) + " of " + strconv.Itoa(total) + " files"
	}
	return line
}

// ── Handlers ──────────────────────────────────────────────────────────────────

// ManagePage handles GET /manage/{token}.
func ManagePage(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := chi.URLParam(r, "token")
		settings := appMiddleware.GetSettings(r)
		m, err := lookupManaged(stores, tok)
		if err != nil {
			slog.Error("manage: lookup", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		if !m.found() {
			renderManageNotFound(w, settings)
			return
		}
		d, err := buildManagePage(cfg, stores, settings, tok, m)
		if err != nil {
			slog.Error("manage: build page", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		d.Extended = r.URL.Query().Get("extended") == "1"
		renderPage(w, "manage.html", d)
	}
}

// ManageExtend handles POST /manage/{token}/extend: the new expiry is now
// plus one of expiry_options, and must be later than the current one.
// Recipients are not mailed; their links simply keep working longer.
func ManageExtend(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := chi.URLParam(r, "token")
		settings := appMiddleware.GetSettings(r)
		m, err := lookupManaged(stores, tok)
		if err != nil {
			slog.Error("manage: lookup", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		if !m.found() {
			renderManageNotFound(w, settings)
			return
		}

		fail := func(msg string) {
			d, err := buildManagePage(cfg, stores, settings, tok, m)
			if err != nil {
				slog.Error("manage: build page", "error", err)
				http.Error(w, "Internal server error", http.StatusInternalServerError)
				return
			}
			d.Error = msg
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			renderPage(w, "manage.html", d)
		}

		hours, err := strconv.Atoi(r.FormValue("expiry_hours"))
		if err != nil || !isValidExpiryOption(hours, cfg.ExpiryOptions) {
			fail("Invalid expiry option.")
			return
		}
		expiresAt := time.Now().Add(time.Duration(hours) * time.Hour)
		if !expiresAt.After(m.expiresAt().Add(minExtension)) {
			fail("That would not make it available any longer than it already is.")
			return
		}

		var extended bool
		if m.Transfer != nil {
			extended, err = stores.Transfers.ExtendExpiry(m.Transfer.ID, expiresAt)
		} else {
			extended, err = stores.Requests.ExtendExpiry(m.Request.ID, expiresAt)
		}
		if err != nil {
			slog.Error("manage: extend", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		if !extended {
			fail("That would not make it available any longer than it already is.")
			return
		}
		if m.Transfer != nil {
			slog.Info("manage: transfer extended", "transfer_id", m.Transfer.ID, "expires_at", expiresAt.Unix())
		} else {
			slog.Info("manage: upload request extended", "request_id", m.Request.ID, "expires_at", expiresAt.Unix())
		}
		http.Redirect(w, r, "/manage/"+tok+"?extended=1", http.StatusSeeOther)
	}
}

// ManageDelete handles POST /manage/{token}/delete: deletes the transfer or
// request at once, through the same code as the admin delete.
func ManageDelete(cfg *config.Config, stores *store.Stores, mgr *storage.Manager, summaries deletionSummarizer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := chi.URLParam(r, "token")
		settings := appMiddleware.GetSettings(r)
		m, err := lookupManaged(stores, tok)
		if err != nil {
			slog.Error("manage: lookup", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		if !m.found() {
			renderManageNotFound(w, settings)
			return
		}

		title, message := "Transfer deleted", "The transfer and its files are deleted. The download links no longer work."
		if m.Transfer != nil {
			err = deleteTransferNow(stores, mgr, summaries, m.Transfer.ID, true)
		} else {
			title, message = "Upload request deleted", "The upload request and the files received are deleted. Its links no longer work."
			err = deleteRequestNow(stores, mgr, m.Request.ID)
		}
		if err != nil {
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		if m.Transfer != nil {
			slog.Info("manage: transfer deleted by sender", "transfer_id", m.Transfer.ID)
		} else {
			slog.Info("manage: upload request deleted by requester", "request_id", m.Request.ID)
		}
		renderPage(w, "not_found.html", struct {
			baseData
			Message string
		}{
			baseData: baseData{PageTitle: title, Settings: settings},
			Message:  message,
		})
	}
}

// renderManageNotFound: unknown token, or the transfer or request expired
// or was deleted. Like the download page, it does not say which.
func renderManageNotFound(w http.ResponseWriter, settings *store.Settings) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	renderPage(w, "not_found.html", struct {
		baseData
		Message string
	}{
		baseData: baseData{PageTitle: "Link not available", Settings: settings},
		Message:  "This manage link has expired or does not exist. Expired transfers and requests cannot be managed any more.",
	})
}
