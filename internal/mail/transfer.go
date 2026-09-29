package mail

// The mail a transfer recipient gets: "X shared N files with you". Sent when
// the transfer goes live, and to a recipient the sender adds later on the
// manage page.

import (
	"fmt"
	"html"
	"log/slog"
	"strings"
	"time"

	"github.com/BartVermeir/Ferri/internal/store"
)

// Transfer holds everything the transfer mails show.
type Transfer struct {
	SenderName  string
	SenderEmail string
	Title       string
	Message     string
	Files       []FileItem
	ExpiresAt   time.Time
	Password    bool
	Loc         *time.Location
	BaseURL     string
	Settings    *store.Settings
	ManageURL   string // sender's manage page; empty for transfers from before migration 006
}

// NewTransfer fills a Transfer from the database. Only complete files are
// listed: a stray 'uploading' row (a TUS client that restarted an upload
// after a 404) is not part of what the recipients receive.
func NewTransfer(t *store.Transfer, files []store.File, settings *store.Settings, loc *time.Location, baseURL string) Transfer {
	m := Transfer{
		SenderName:  t.SenderName,
		SenderEmail: t.SenderEmail,
		Title:       t.Title,
		Message:     t.Message,
		ExpiresAt:   t.ExpiresAt,
		Password:    t.PasswordHash.Valid,
		Loc:         loc,
		BaseURL:     baseURL,
		Settings:    settings,
	}
	if t.ManageToken.Valid && t.ManageToken.String != "" {
		m.ManageURL = baseURL + "/manage/" + t.ManageToken.String
	}
	for _, f := range files {
		if f.Status == "complete" {
			m.Files = append(m.Files, FileItem{Name: f.OriginalName, Size: f.SizeBytes})
		}
	}
	return m
}

// Sender is the name, or the address when the (optional) name is empty.
func (m Transfer) Sender() string {
	if m.SenderName != "" {
		return m.SenderName
	}
	if m.SenderEmail != "" {
		return m.SenderEmail
	}
	return "Someone"
}

// SubjectTitle is the quoted title, or "N files" when the sender left the
// (optional) title empty.
func (m Transfer) SubjectTitle() string {
	if m.Title != "" {
		return `"` + m.Title + `"`
	}
	return Plural(len(m.Files), "file")
}

// EnqueueAvailable queues the "shared with you" mail for each recipient
// that has not had it yet. Claiming the recipient first makes it safe when
// the transfer goes live while the sender adds someone: whoever claims it
// sends the one mail.
func EnqueueAvailable(stores *store.Stores, m Transfer, recipients []store.Recipient) {
	subject := fmt.Sprintf("%s shared %s with you", m.Sender(), m.SubjectTitle())
	for _, r := range recipients {
		claimed, err := stores.Transfers.ClaimRecipientNotify(r.ID)
		if err != nil {
			slog.Error("mail: claim recipient", "recipient_id", r.ID, "error", err)
			continue
		}
		if !claimed {
			continue
		}
		downloadURL := m.BaseURL + "/dl/" + r.DownloadToken
		if err := stores.Mail.Enqueue(nil, store.MailAbout{TransferID: r.TransferID}, r.Email, subject, AvailableHTML(m, downloadURL), AvailableText(m, downloadURL)); err != nil {
			slog.Error("mail: enqueue recipient mail", "recipient_id", r.ID, "error", err)
		}
	}
}

func AvailableHTML(m Transfer, downloadURL string) string {
	company := CompanyName(m.Settings)

	from := "<strong>" + html.EscapeString(m.Sender()) + "</strong>"
	if m.SenderName != "" && m.SenderEmail != "" {
		from += ` (<a href="mailto:` + html.EscapeString(m.SenderEmail) + `" style="color:#555;">` + html.EscapeString(m.SenderEmail) + `</a>)`
	}

	var b strings.Builder
	b.WriteString(`<p style="margin:0 0 16px;">Hello,</p>`)
	fmt.Fprintf(&b, `<p style="margin:0 0 20px;">%s has shared %s with you through %s.</p>`,
		from, Plural(len(m.Files), "file"), html.EscapeString(company))
	if m.Title != "" {
		fmt.Fprintf(&b, `<p style="margin:0 0 12px;font-size:18px;font-weight:600;color:#1a1a1a;line-height:1.3;">%s</p>`, html.EscapeString(m.Title))
	}
	b.WriteString(QuoteHTML(m.Message))
	b.WriteString(FileListHTML(m.Files))
	b.WriteString(ButtonHTML(downloadURL, "Download files", m.Settings))
	fmt.Fprintf(&b, `<p style="margin:0 0 8px;font-size:13px;color:#555;">The files are available until <strong>%s</strong>. After that date the link stops working.</p>`,
		html.EscapeString(FormatDate(m.ExpiresAt, m.Loc)))
	if m.Password {
		fmt.Fprintf(&b, `<p style="margin:0 0 8px;font-size:13px;color:#555;">This transfer is password protected. You need the password from %s to open it.</p>`,
			html.EscapeString(m.Sender()))
	}
	b.WriteString(NoteHTML(fmt.Sprintf(
		"You are receiving this email because %s entered your address to send you files. This link is personal to you, please do not forward it. If you were not expecting these files, you can ignore this email.",
		m.Sender())))

	preheader := fmt.Sprintf("%s shared %s with you, available until %s.",
		m.Sender(), Plural(len(m.Files), "file"), FormatDate(m.ExpiresAt, m.Loc))
	return Wrap(m.Settings, m.BaseURL, preheader, b.String())
}

func AvailableText(m Transfer, downloadURL string) string {
	var b strings.Builder
	b.WriteString("Hello,\n\n")
	from := m.Sender()
	if m.SenderName != "" && m.SenderEmail != "" {
		from += " (" + m.SenderEmail + ")"
	}
	fmt.Fprintf(&b, "%s has shared %s with you through %s.\n\n",
		from, Plural(len(m.Files), "file"), CompanyName(m.Settings))
	if m.Title != "" {
		b.WriteString(m.Title + "\n\n")
	}
	if m.Message != "" {
		b.WriteString(m.Message + "\n\n")
	}
	if len(m.Files) > 0 {
		b.WriteString("Files:\n" + FileListText(m.Files) + "\n")
	}
	fmt.Fprintf(&b, "Download: %s\n\n", downloadURL)
	fmt.Fprintf(&b, "The files are available until %s. After that date the link stops working.\n",
		FormatDate(m.ExpiresAt, m.Loc))
	if m.Password {
		fmt.Fprintf(&b, "This transfer is password protected. You need the password from %s to open it.\n", m.Sender())
	}
	fmt.Fprintf(&b, "\n--\nYou are receiving this email because %s entered your address to send you files. This link is personal to you, please do not forward it. If you were not expecting these files, you can ignore this email.\n",
		m.Sender())
	return b.String()
}
