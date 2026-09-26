package jobs

import (
	"fmt"
	"html"
	"log/slog"
	"strings"
	"time"

	"github.com/BartVermeir/Ferri/internal/mail"
	"github.com/BartVermeir/Ferri/internal/store"
)

// Reminder timing: a request with nothing uploaded gets one reminder in its
// last day, but only once it is at least a day old — a request made for one
// day would otherwise be reminded right after it was created.
const (
	reminderBefore = 24 * time.Hour
	reminderMinAge = 24 * time.Hour
)

// sendRequestReminders mails the requester of every open request that
// expires within reminderBefore without a single file received. Runs with
// the expiry job. The upload link goes to external parties by hand, so
// Ferri does not know them: the reminder goes to the requester, with the
// link to forward again and the manage link to extend the request.
func (s *Scheduler) sendRequestReminders() {
	settings := s.stores.Settings.Get()
	if settings.MailFromAddress == "" {
		return
	}
	due, err := s.stores.Requests.GetDueForReminder(reminderBefore, reminderMinAge)
	if err != nil {
		slog.Error("reminder job: get due requests", "error", err)
		return
	}
	sent := 0
	for i := range due {
		r := &due[i]
		claimed, err := s.stores.Requests.ClaimReminder(r.ID)
		if err != nil {
			slog.Error("reminder job: claim", "request", r.ID, "error", err)
			continue
		}
		if !claimed {
			continue
		}
		m := requestReminder{Settings: settings, BaseURL: s.cfg.Server.BaseURL, Loc: s.cfg.Server.Location, Request: r}
		if err := s.stores.Mail.Enqueue(nil, r.RequesterEmail, m.subject(), buildRequestReminderHTML(m), buildRequestReminderText(m)); err != nil {
			slog.Error("reminder job: enqueue", "request", r.ID, "error", err)
			continue
		}
		sent++
	}
	if sent > 0 {
		slog.Info("reminder job: requests reminded", "count", sent)
	}
}

// requestReminder holds what the "nothing uploaded yet" mail shows.
type requestReminder struct {
	Settings *store.Settings
	BaseURL  string
	Loc      *time.Location
	Request  *store.UploadRequest
}

func (m requestReminder) label() string {
	if m.Request.Title != "" {
		return `"` + m.Request.Title + `"`
	}
	return "your upload request"
}

func (m requestReminder) subject() string {
	return "Nothing uploaded yet: " + m.label()
}

func (m requestReminder) uploadURL() string {
	return m.BaseURL + "/ul/" + m.Request.UploadToken
}

// manageURL is empty for a request from before migration 006.
func (m requestReminder) manageURL() string {
	if !m.Request.ManageToken.Valid || m.Request.ManageToken.String == "" {
		return ""
	}
	return m.BaseURL + "/manage/" + m.Request.ManageToken.String
}

func (m requestReminder) intro() string {
	return fmt.Sprintf("Nobody has uploaded files for %s yet. The upload link stops working on %s.",
		m.label(), mail.FormatDate(m.Request.ExpiresAt, m.Loc))
}

const reminderManageWhat = "Need more time? Extend the request, or delete it, on its manage page:"

func (m requestReminder) note() string {
	return "You are receiving this email because you requested files with " + mail.CompanyName(m.Settings) +
		" and nothing was uploaded a day before the link expires. You get this reminder once."
}

func buildRequestReminderHTML(m requestReminder) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<p style="margin:0 0 16px;">Hello %s,</p>`, html.EscapeString(m.Request.RequesterName))
	fmt.Fprintf(&b, `<p style="margin:0 0 20px;">%s</p>`, html.EscapeString(m.intro()))
	b.WriteString(`<p style="margin:0 0 6px;font-size:13px;color:#555;">If you still need the files, send this link again to the person you asked:</p>`)
	fmt.Fprintf(&b, `<p style="margin:0 0 20px;padding:10px 12px;background:#f7f7f5;border-radius:6px;font-size:13px;word-break:break-all;"><a href="%s" style="color:#333;">%s</a></p>`,
		html.EscapeString(m.uploadURL()), html.EscapeString(m.uploadURL()))
	if u := m.manageURL(); u != "" {
		b.WriteString(mail.ManageLinkHTML(u, reminderManageWhat))
	}
	b.WriteString(mail.NoteHTML(m.note()))
	return mail.Wrap(m.Settings, m.BaseURL, m.intro(), b.String())
}

func buildRequestReminderText(m requestReminder) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Hello %s,\n\n%s\n\n", m.Request.RequesterName, m.intro())
	fmt.Fprintf(&b, "If you still need the files, send this link again to the person you asked:\n%s\n\n", m.uploadURL())
	if u := m.manageURL(); u != "" {
		b.WriteString(mail.ManageLinkText(u, reminderManageWhat) + "\n")
	}
	fmt.Fprintf(&b, "--\n%s\n", m.note())
	return b.String()
}
