package jobs

import (
	"fmt"
	"html"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/BartVermeir/Ferri/internal/mail"
	"github.com/BartVermeir/Ferri/internal/store"
)

// Reminder timing: a request with nothing uploaded gets one reminder in its
// last day, but only once it is at least a day old: a request made for one
// day gets none.
const (
	reminderBefore = 24 * time.Hour
	reminderMinAge = 24 * time.Hour
)

// sendRequestReminders mails the requester of every open request that
// expires within reminderBefore without a single file received. Runs with
// the expiry job. The upload link goes to external parties by hand, so
// Ferri does not know them: the reminder goes to the requester, with the
// link to forward again and the manage link to extend the request. Requests
// of the same requester due in one run go in one mail ("Number of links"
// makes many at once).
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
	var order []string
	groups := map[string][]*store.UploadRequest{}
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
		key := strings.ToLower(r.RequesterEmail)
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], r)
	}
	requests, mails := 0, 0
	for _, key := range order {
		reqs := groups[key]
		var err error
		if len(reqs) == 1 {
			m := requestReminder{Settings: settings, BaseURL: s.cfg.Server.BaseURL, Loc: s.cfg.Server.Location, Request: reqs[0]}
			err = s.stores.Mail.Enqueue(nil, store.MailAbout{RequestID: reqs[0].ID}, reqs[0].RequesterEmail, m.subject(), buildRequestReminderHTML(m), buildRequestReminderText(m))
		} else {
			m := newRequestReminders(settings, s.cfg.Server.BaseURL, s.cfg.Server.Location, reqs)
			err = s.stores.Mail.Enqueue(nil, store.MailAbout{}, reqs[0].RequesterEmail, m.subject(), buildRequestRemindersHTML(m), buildRequestRemindersText(m))
		}
		if err != nil {
			slog.Error("reminder job: enqueue", "requests", len(reqs), "error", err)
			continue
		}
		requests += len(reqs)
		mails++
	}
	if requests > 0 {
		slog.Info("reminder job: requests reminded", "requests", requests, "mails", mails)
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

// manageURL is empty for a request without a manage token.
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

const (
	reminderManageWhat  = "Need more time? Extend the request, or delete it, on its manage page:"
	reminderManageLabel = "Manage request"
)

func (m requestReminder) note() string {
	return "You are receiving this email because you requested files with " + mail.CompanyName(m.Settings) +
		" and nothing was uploaded a day before the link expires. You get this reminder once."
}

func buildRequestReminderHTML(m requestReminder) string {
	var b strings.Builder
	b.WriteString(mail.HelloHTML(m.Request.RequesterName))
	fmt.Fprintf(&b, `<p style="margin:0 0 20px;">%s</p>`, html.EscapeString(m.intro()))
	b.WriteString(`<p style="margin:0 0 6px;font-size:13px;color:#555;">If you still need the files, send this link again to the person you asked:</p>`)
	fmt.Fprintf(&b, `<p style="margin:0 0 20px;padding:10px 12px;background:#f7f7f5;border-radius:6px;font-size:13px;word-break:break-all;"><a href="%s" style="color:#333;">%s</a></p>`,
		html.EscapeString(m.uploadURL()), html.EscapeString(m.uploadURL()))
	if u := m.manageURL(); u != "" {
		fmt.Fprintf(&b, `<p style="margin:0 0 12px;font-size:13px;color:#555;">%s</p>`, reminderManageWhat)
		b.WriteString(mail.ButtonsHTML("", "", u, reminderManageLabel, m.Settings))
	}
	b.WriteString(mail.NoteHTML(m.note()))
	return mail.Wrap(m.Settings, m.BaseURL, m.intro(), b.String())
}

func buildRequestReminderText(m requestReminder) string {
	var b strings.Builder
	b.WriteString(mail.HelloText(m.Request.RequesterName) + m.intro() + "\n\n")
	fmt.Fprintf(&b, "If you still need the files, send this link again to the person you asked:\n%s\n\n", m.uploadURL())
	if u := m.manageURL(); u != "" {
		b.WriteString(reminderManageWhat + "\n" + mail.ManageLinkText(u, reminderManageLabel) + "\n")
	}
	fmt.Fprintf(&b, "--\n%s\n", m.note())
	return b.String()
}

// requestReminders holds what the combined "nothing uploaded yet" mail shows,
// for several requests of one requester, by expiry and title.
type requestReminders struct {
	Settings *store.Settings
	BaseURL  string
	Loc      *time.Location
	Requests []*store.UploadRequest
}

func newRequestReminders(settings *store.Settings, baseURL string, loc *time.Location, reqs []*store.UploadRequest) requestReminders {
	sorted := append([]*store.UploadRequest(nil), reqs...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if !a.ExpiresAt.Equal(b.ExpiresAt) {
			return a.ExpiresAt.Before(b.ExpiresAt)
		}
		if len(a.Title) != len(b.Title) {
			return len(a.Title) < len(b.Title) // "Title #2" before "Title #10"
		}
		return a.Title < b.Title
	})
	return requestReminders{Settings: settings, BaseURL: baseURL, Loc: loc, Requests: sorted}
}

// item is the reminder of one request in the list.
func (m requestReminders) item(r *store.UploadRequest) requestReminder {
	return requestReminder{Settings: m.Settings, BaseURL: m.BaseURL, Loc: m.Loc, Request: r}
}

func (m requestReminders) subject() string {
	return fmt.Sprintf("Nothing uploaded yet: %d upload requests", len(m.Requests))
}

func (m requestReminders) intro() string {
	first := mail.FormatDate(m.Requests[0].ExpiresAt, m.Loc)
	last := mail.FormatDate(m.Requests[len(m.Requests)-1].ExpiresAt, m.Loc)
	when := "on " + first
	if first != last {
		when = "between " + first + " and " + last
	}
	return fmt.Sprintf("Nobody has uploaded files yet for %d of your upload requests. Their upload links stop working %s.",
		len(m.Requests), when)
}

// reminderManagePagesNote goes under the list of manage links.
const reminderManagePagesNote = "The manage pages only open from the internal network."

func (m requestReminders) note() string {
	return "You are receiving this email because you requested files with " + mail.CompanyName(m.Settings) +
		" and nothing was uploaded a day before the links expire. You get this reminder once per request."
}

func buildRequestRemindersHTML(m requestReminders) string {
	var b strings.Builder
	b.WriteString(mail.HelloHTML(m.Requests[0].RequesterName))
	fmt.Fprintf(&b, `<p style="margin:0 0 16px;">%s</p>`, html.EscapeString(m.intro()))
	b.WriteString(`<p style="margin:0 0 12px;font-size:13px;color:#555;">If you still need the files, send the link again to the person you asked. To extend or delete a request, use its manage page.</p>`)
	for _, r := range m.Requests {
		it := m.item(r)
		fmt.Fprintf(&b, `<p style="margin:0 0 14px;padding:10px 12px;background:#f7f7f5;border-radius:6px;font-size:13px;word-break:break-all;"><strong>%s</strong> &middot; stops working on %s<br>Upload link: <a href="%s" style="color:#333;">%s</a>`,
			html.EscapeString(it.label()), html.EscapeString(mail.FormatDate(r.ExpiresAt, m.Loc)),
			html.EscapeString(it.uploadURL()), html.EscapeString(it.uploadURL()))
		if u := it.manageURL(); u != "" {
			fmt.Fprintf(&b, `<br>%s: <a href="%s" style="color:#333;">%s</a>`,
				reminderManageLabel, html.EscapeString(u), html.EscapeString(u))
		}
		b.WriteString(`</p>`)
	}
	fmt.Fprintf(&b, `<p style="margin:0 0 20px;font-size:12px;color:#888;">%s</p>`, reminderManagePagesNote)
	b.WriteString(mail.NoteHTML(m.note()))
	return mail.Wrap(m.Settings, m.BaseURL, m.intro(), b.String())
}

func buildRequestRemindersText(m requestReminders) string {
	var b strings.Builder
	b.WriteString(mail.HelloText(m.Requests[0].RequesterName) + m.intro() + "\n\n")
	b.WriteString("If you still need the files, send the link again to the person you asked. To extend or delete a request, use its manage page.\n\n")
	for _, r := range m.Requests {
		it := m.item(r)
		fmt.Fprintf(&b, "%s - stops working on %s\nUpload link: %s\n", it.label(), mail.FormatDate(r.ExpiresAt, m.Loc), it.uploadURL())
		if u := it.manageURL(); u != "" {
			fmt.Fprintf(&b, "%s: %s\n", reminderManageLabel, u)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "(%s)\n\n--\n%s\n", reminderManagePagesNote, m.note())
	return b.String()
}
