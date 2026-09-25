package mail

import (
	"fmt"
	"html"
	"net/url"
	"strings"

	"github.com/BartVermeir/Ferri/internal/store"
)

// Wrap renders bodyHTML inside the standard Ferri mail layout.
//
// Uses branding settings (logo, primary color, company name) from the DB.
// baseURL turns a site-relative logo path (/static/logo/logo.png) into an
// absolute URL — a mail client has no page to resolve a relative src against.
// preheader is the one-line preview most clients show next to the subject;
// leaving it empty makes them show the first body text instead, which for a
// logo-first layout is often just the company name.
func Wrap(settings *store.Settings, baseURL, preheader, bodyHTML string) string {
	primary := "#1a1a1a"
	company := "Ferri"
	if settings != nil {
		if settings.PrimaryColor != "" {
			primary = settings.PrimaryColor
		}
		if settings.CompanyName != "" {
			company = settings.CompanyName
		}
	}
	companyHTML := html.EscapeString(company)

	header := fmt.Sprintf(`<span style="font-size:15px;font-weight:600;color:%s;letter-spacing:0.01em;">%s</span>`,
		html.EscapeString(primary), companyHTML)
	if src := logoSrc(settings, baseURL); src != "" {
		header = fmt.Sprintf(`<img src="%s" alt="%s" height="40" style="display:block;height:40px;width:auto;max-width:220px;border:0;outline:none;text-decoration:none;">`,
			html.EscapeString(src), companyHTML)
	}

	footerLink := ""
	if host := siteHost(baseURL); host != "" {
		footerLink = fmt.Sprintf(` · <a href="%s" style="color:#999;text-decoration:underline;">%s</a>`,
			html.EscapeString(baseURL), html.EscapeString(host))
	}

	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="color-scheme" content="light">
<title>%s</title>
</head>
<body style="margin:0;padding:0;background:#f4f4f2;font-family:system-ui,-apple-system,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;color:#1a1a1a;">
<div style="display:none;max-height:0;overflow:hidden;opacity:0;color:transparent;">%s</div>
<table role="presentation" width="100%%" cellpadding="0" cellspacing="0" style="background:#f4f4f2;padding:32px 16px;">
  <tr><td align="center">
    <table role="presentation" width="560" cellpadding="0" cellspacing="0" style="max-width:560px;width:100%%;">
      <tr><td style="background:#fff;padding:24px 32px 0;border-radius:8px 8px 0 0;border:1px solid #e8e8e4;border-bottom:none;">
        %s
      </td></tr>
      <tr><td style="background:#fff;padding:0 32px;border-left:1px solid #e8e8e4;border-right:1px solid #e8e8e4;">
        <div style="height:2px;background:%s;margin-top:18px;opacity:0.25;"></div>
      </td></tr>
      <tr><td style="background:#fff;padding:24px 32px 32px;border-radius:0 0 8px 8px;border:1px solid #e8e8e4;border-top:none;font-size:14px;line-height:1.6;color:#333;">
        %s
      </td></tr>
      <tr><td style="padding:16px 8px;text-align:center;color:#999;font-size:11px;line-height:1.5;">
        %s secure file transfer%s
      </td></tr>
    </table>
  </td></tr>
</table>
</body>
</html>`, companyHTML, html.EscapeString(preheader), header, html.EscapeString(primary), bodyHTML, companyHTML, footerLink)
}

// logoSrc returns the absolute logo URL for use in a mail, or "" when there
// is no logo or it cannot be made absolute (relative path without baseURL).
func logoSrc(settings *store.Settings, baseURL string) string {
	if settings == nil || settings.LogoURL == "" {
		return ""
	}
	logo := settings.LogoURL
	if strings.HasPrefix(logo, "https://") || strings.HasPrefix(logo, "http://") {
		return logo
	}
	if strings.HasPrefix(logo, "/") && baseURL != "" {
		return strings.TrimRight(baseURL, "/") + logo
	}
	return ""
}

func siteHost(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	return u.Host
}
