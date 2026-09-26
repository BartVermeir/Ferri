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

// Admin alerts: problems that used to show up only as WARN lines in the logs
// are mailed to the addresses under "Alerts" in the admin settings. At most
// one mail per kind per alertRepeatAfter; nothing is checked while no address
// is set.
const (
	alertInterval    = 15 * time.Minute
	alertRepeatAfter = 24 * time.Hour
	// A storage check that fails once can be an SMB reconnect; only a
	// failure that lasts this long is mailed.
	storageDownAfter = 30 * time.Minute
	// Deleted files that the cleanup job (every 6 h) could not remove for
	// this long are mailed: by then several retries failed.
	leftoversAfter = 24 * time.Hour
	// maxAlertLines caps the failed mails listed in one alert.
	maxAlertLines = 20
)

// alertSubjectPrefix starts every alert subject. FailedSince skips mails
// with it: a failed alert mail would otherwise report itself every day.
const alertSubjectPrefix = "[Alert] "

// Alert kinds, the keys in alert_state.
const (
	alertStorageDown = "storage_down"
	alertLowSpace    = "low_space"
	alertLeftovers   = "leftovers"
	alertMailFailed  = "mail_failed"
)

// alertMail is one alert: what happened, the details and what to do.
type alertMail struct {
	Kind    string
	Subject string
	Intro   string
	Details []string
	Todo    string
}

// runAlertJob checks every alert condition and mails what is due.
func (s *Scheduler) runAlertJob() {
	settings := s.stores.Settings.Get()
	to := settings.AlertRecipientList()
	if len(to) == 0 || settings.MailFromAddress == "" {
		return
	}
	now := time.Now()

	var alerts []alertMail
	add := func(a *alertMail) {
		if a != nil {
			alerts = append(alerts, *a)
		}
	}
	a, storageUp := s.checkStorage(now)
	add(a)
	if storageUp { // a storage that is down has no free space to read
		add(s.checkFreeSpace(now))
	}
	add(s.checkLeftovers(now))
	add(s.checkFailedMails(now))

	for _, a := range alerts {
		subject := alertSubjectPrefix + mail.CompanyName(settings) + ": " + a.Subject
		bodyHTML := buildAlertHTML(settings, s.cfg.Server.BaseURL, a)
		bodyText := buildAlertText(settings, s.cfg.Server.BaseURL, a)
		queued := false
		for _, addr := range to {
			if err := s.stores.Mail.Enqueue(nil, addr, subject, bodyHTML, bodyText); err != nil {
				slog.Error("alert job: enqueue", "kind", a.Kind, "to", addr, "error", err)
				continue
			}
			queued = true
		}
		if !queued {
			continue
		}
		if err := s.stores.Alerts.MarkSent(a.Kind, now); err != nil {
			slog.Error("alert job: mark sent", "kind", a.Kind, "error", err)
		}
		slog.Warn("alert job: alert mailed", "kind", a.Kind, "recipients", len(to))
	}
}

// track records whether a condition holds and returns its state, or ok false
// when the state could not be read or written (logged).
func (s *Scheduler) track(kind string, active bool) (store.AlertState, bool) {
	var err error
	if active {
		err = s.stores.Alerts.SetActive(kind)
	} else {
		err = s.stores.Alerts.Clear(kind)
	}
	if err != nil {
		slog.Error("alert job: save state", "kind", kind, "error", err)
		return store.AlertState{}, false
	}
	st, err := s.stores.Alerts.Get(kind)
	if err != nil {
		slog.Error("alert job: read state", "kind", kind, "error", err)
		return store.AlertState{}, false
	}
	return st, true
}

// due reports whether an active condition should be mailed now: it lasted at
// least `after`, and the last alert of its kind is alertRepeatAfter old.
func due(st store.AlertState, now time.Time, after time.Duration) bool {
	if st.Since.IsZero() || now.Sub(st.Since) < after {
		return false
	}
	return st.LastSentAt.IsZero() || now.Sub(st.LastSentAt) >= alertRepeatAfter
}

// checkStorage tests that the storage is reachable and writable. Returns an
// alert when it failed for storageDownAfter, and whether the storage is up.
func (s *Scheduler) checkStorage(now time.Time) (*alertMail, bool) {
	testErr := s.mgr.TestConnection()
	st, ok := s.track(alertStorageDown, testErr != nil)
	if testErr == nil || !ok || !due(st, now, storageDownAfter) {
		return nil, testErr == nil
	}
	return &alertMail{
		Kind:    alertStorageDown,
		Subject: "storage not reachable",
		Intro: fmt.Sprintf("Ferri cannot reach or write to the storage (%s) since %s. Uploads and downloads fail while this lasts.",
			s.mgr.Type(), mail.FormatDate(st.Since, s.cfg.Server.Location)),
		Details: []string{"Error: " + testErr.Error()},
		Todo:    `Check the SMB share or the storage folder, and the storage settings in the admin panel ("Test connection").`,
	}, false
}

// checkFreeSpace alerts when the storage has less than twice
// limits.min_free_bytes free: below min_free_bytes every new upload is
// refused, so this gives time to act. A backend that cannot report its free
// space is logged, not alerted (uploads go ahead there too).
func (s *Scheduler) checkFreeSpace(now time.Time) *alertMail {
	free, err := s.mgr.FreeSpace()
	if err != nil {
		slog.Warn("alert job: cannot read free space on storage", "error", err)
		return nil
	}
	minFree := uint64(s.cfg.Limits.MinFreeBytes)
	threshold := 2 * minFree
	st, ok := s.track(alertLowSpace, free < threshold)
	if free >= threshold || !ok || !due(st, now, 0) {
		return nil
	}
	return &alertMail{
		Kind:    alertLowSpace,
		Subject: "storage almost full",
		Intro: fmt.Sprintf("The storage has %s free. Below %s Ferri refuses new uploads, and each upload also needs its own size free on top of that.",
			mail.FormatSize(int64(free)), mail.FormatSize(int64(minFree))),
		Details: []string{fmt.Sprintf("Alert threshold: %s (twice limits.min_free_bytes)", mail.FormatSize(int64(threshold)))},
		Todo:    `Free up space on the storage, or remove transfers that are no longer needed. Expired transfers are deleted after the grace period; "Force cleanup" in the admin panel does it now.`,
	}
}

// checkLeftovers alerts when deleted files are still on storage after
// leftoversAfter: the cleanup job retries them every run (purgeLeftovers),
// so by then removing them keeps failing.
func (s *Scheduler) checkLeftovers(now time.Time) *alertMail {
	list, err := s.stores.Files.ListUnpurged()
	if err != nil {
		slog.Error("alert job: list unpurged files", "error", err)
		return nil
	}
	st, ok := s.track(alertLeftovers, len(list) > 0)
	if len(list) == 0 || !ok || !due(st, now, leftoversAfter) {
		return nil
	}
	var total int64
	for _, u := range list {
		total += u.SizeBytes
	}
	return &alertMail{
		Kind:    alertLeftovers,
		Subject: "deleted files still on storage",
		Intro: fmt.Sprintf("%s (%s) are marked deleted but are still on the storage, since at least %s. The cleanup job retries them on every run, and it keeps failing.",
			mail.Plural(len(list), "deleted file"), mail.FormatSize(total), mail.FormatDate(st.Since, s.cfg.Server.Location)),
		Todo: `Look for "remove file failed" in the logs, and check the permissions on the storage: the container runs as UID 1000.`,
	}
}

// checkFailedMails alerts on mails that failed for good since the last alert
// of this kind. Alert mails themselves are left out. If SMTP itself is down
// this alert cannot be delivered either; the mail queue in the admin panel
// still shows the failures.
func (s *Scheduler) checkFailedMails(now time.Time) *alertMail {
	st, err := s.stores.Alerts.Get(alertMailFailed)
	if err != nil {
		slog.Error("alert job: read state", "kind", alertMailFailed, "error", err)
		return nil
	}
	if !st.LastSentAt.IsZero() && now.Sub(st.LastSentAt) < alertRepeatAfter {
		return nil // the failures wait for the next alert, from LastSentAt on
	}
	failed, err := s.stores.Mail.FailedSince(st.LastSentAt, alertSubjectPrefix)
	if err != nil {
		slog.Error("alert job: list failed mails", "error", err)
		return nil
	}
	if len(failed) == 0 {
		return nil
	}
	a := &alertMail{
		Kind:    alertMailFailed,
		Subject: mail.Plural(len(failed), "mail") + " could not be sent",
		Intro:   fmt.Sprintf("%s failed for good, after every retry:", mail.Plural(len(failed), "mail")),
		Todo:    "Check the addresses and the errors. The mail queue in the admin panel can send them again.",
	}
	for i, m := range failed {
		if i == maxAlertLines {
			a.Details = append(a.Details, fmt.Sprintf("… and %d more", len(failed)-maxAlertLines))
			break
		}
		a.Details = append(a.Details, fmt.Sprintf("To %s, %q: %s", m.ToAddress, m.Subject, m.ErrorMessage.String))
	}
	return a
}

func alertNote(settings *store.Settings) string {
	return "You are receiving this email because your address is listed under Alerts in the admin settings of " +
		mail.CompanyName(settings) + ". You get at most one alert of this kind per 24 hours."
}

func buildAlertHTML(settings *store.Settings, baseURL string, a alertMail) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<p style="margin:0 0 16px;">%s</p>`, html.EscapeString(a.Intro))
	if len(a.Details) > 0 {
		b.WriteString(`<ul style="margin:0 0 16px;padding-left:18px;font-size:13px;color:#555;line-height:1.6;">`)
		for _, d := range a.Details {
			fmt.Fprintf(&b, `<li style="word-break:break-all;">%s</li>`, html.EscapeString(d))
		}
		b.WriteString(`</ul>`)
	}
	fmt.Fprintf(&b, `<p style="margin:0 0 20px;">%s</p>`, html.EscapeString(a.Todo))
	b.WriteString(mail.ButtonHTML(baseURL+"/admin", "Open the admin panel", settings))
	b.WriteString(mail.NoteHTML(alertNote(settings)))
	return mail.Wrap(settings, baseURL, a.Intro, b.String())
}

func buildAlertText(settings *store.Settings, baseURL string, a alertMail) string {
	var b strings.Builder
	b.WriteString(a.Intro + "\n\n")
	for _, d := range a.Details {
		b.WriteString("  - " + d + "\n")
	}
	if len(a.Details) > 0 {
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "%s\n\nAdmin panel: %s/admin\n\n--\n%s\n", a.Todo, baseURL, alertNote(settings))
	return b.String()
}
