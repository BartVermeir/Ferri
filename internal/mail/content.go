package mail

// Shared building blocks for mail bodies. Every builder uses these so all
// mails look alike and the HTML and plain-text parts carry the same content:
// a text part that says much less than the HTML part is a spam signal.

import (
	"fmt"
	"html"
	"strconv"
	"strings"
	"time"

	"github.com/BartVermeir/Ferri/internal/store"
)

// FileItem is one line in a mail's file list.
type FileItem struct {
	Name string
	Size int64
}

// FormatSize renders a byte count the same way the web UI does (1.5 MB).
func FormatSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// FormatDate renders t in loc as "Mon 2 Jan 2006, 15:04".
func FormatDate(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format("Mon 2 Jan 2006, 15:04")
}

// Plural returns "1 file" / "3 files".
func Plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// PrimaryColor returns the branding primary color or the default.
func PrimaryColor(settings *store.Settings) string {
	if settings != nil && settings.PrimaryColor != "" {
		return settings.PrimaryColor
	}
	return "#1a1a1a"
}

// CompanyName returns the branding company name or "Ferri".
func CompanyName(settings *store.Settings) string {
	if settings != nil && settings.CompanyName != "" {
		return settings.CompanyName
	}
	return "Ferri"
}

// FileListHTML renders files as a two-column table with a total row.
func FileListHTML(files []FileItem) string {
	if len(files) == 0 {
		return ""
	}
	var b strings.Builder
	var total int64
	b.WriteString(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="border-collapse:collapse;margin:0 0 20px;">`)
	for _, f := range files {
		total += f.Size
		fmt.Fprintf(&b,
			`<tr><td style="padding:7px 0;font-size:13px;color:#333;border-bottom:1px solid #f0f0ec;word-break:break-all;">%s</td>`+
				`<td style="padding:7px 0 7px 12px;font-size:13px;color:#888;text-align:right;white-space:nowrap;border-bottom:1px solid #f0f0ec;">%s</td></tr>`,
			html.EscapeString(f.Name), FormatSize(f.Size))
	}
	if len(files) > 1 {
		fmt.Fprintf(&b,
			`<tr><td style="padding:7px 0;font-size:13px;color:#888;">%s</td>`+
				`<td style="padding:7px 0 7px 12px;font-size:13px;color:#888;text-align:right;white-space:nowrap;">%s total</td></tr>`,
			Plural(len(files), "file"), FormatSize(total))
	}
	b.WriteString(`</table>`)
	return b.String()
}

// FileListText is the plain-text counterpart of FileListHTML.
func FileListText(files []FileItem) string {
	var b strings.Builder
	var total int64
	for _, f := range files {
		total += f.Size
		fmt.Fprintf(&b, "  - %s (%s)\n", f.Name, FormatSize(f.Size))
	}
	if len(files) > 1 {
		fmt.Fprintf(&b, "  %s, %s total\n", Plural(len(files), "file"), FormatSize(total))
	}
	return b.String()
}

// ButtonHTML renders a call-to-action button followed by the same URL as
// readable text. The visible URL lets recipients see where the link goes and
// still works in clients that strip button styling; filters also score a
// mail whose only link hides behind a button slightly worse.
func ButtonHTML(url, label string, settings *store.Settings) string {
	primary := PrimaryColor(settings)
	return fmt.Sprintf(`<table role="presentation" cellpadding="0" cellspacing="0" style="margin:4px 0 12px;">
  <tr><td style="border-radius:6px;background:%s;">
    <a href="%s" style="display:inline-block;padding:12px 24px;color:%s;text-decoration:none;border-radius:6px;font-size:14px;font-weight:600;">%s</a>
  </td></tr>
</table>
<p style="margin:0 0 20px;font-size:12px;color:#888;line-height:1.5;">Or copy this link into your browser:<br><a href="%s" style="color:#555;word-break:break-all;">%s</a></p>`,
		html.EscapeString(primary), html.EscapeString(url), contrastText(primary), html.EscapeString(label),
		html.EscapeString(url), html.EscapeString(url))
}

// QuoteHTML renders a user-written message as a quoted block.
func QuoteHTML(message string) string {
	if message == "" {
		return ""
	}
	return fmt.Sprintf(`<div style="margin:0 0 20px;padding:12px 16px;background:#f7f7f5;border-left:3px solid #ddd;font-size:14px;color:#444;line-height:1.6;white-space:pre-line;">%s</div>`,
		html.EscapeString(message))
}

// NoteHTML renders a small grey closing paragraph (why you got this mail).
func NoteHTML(text string) string {
	return fmt.Sprintf(`<p style="margin:24px 0 0;padding-top:16px;border-top:1px solid #f0f0ec;font-size:12px;color:#999;line-height:1.5;">%s</p>`,
		html.EscapeString(text))
}

// contrastText picks white or black text for a hex background, with the same
// formula as the --primary-text calculation in static/files/base.js.
func contrastText(hex string) string {
	h := strings.TrimPrefix(hex, "#")
	if len(h) == 3 {
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	}
	if len(h) != 6 {
		return "#ffffff"
	}
	v, err := strconv.ParseUint(h, 16, 32)
	if err != nil {
		return "#ffffff"
	}
	r, g, b := float64(v>>16&0xff)/255, float64(v>>8&0xff)/255, float64(v&0xff)/255
	if 0.2126*r+0.7152*g+0.0722*b > 0.5 {
		return "#000000"
	}
	return "#ffffff"
}
