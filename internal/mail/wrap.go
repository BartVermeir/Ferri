package mail

import (
	"fmt"
	"html"

	"github.com/BartVermeir/Ferri/internal/store"
)

// Wrap renders bodyHTML inside the standard Ferri mail layout.
// Uses branding settings (primary color, company name) from the DB.
func Wrap(settings *store.Settings, bodyHTML string) string {
	primary := "#1a1a1a"
	company := "Ferri"
	if settings != nil {
		if settings.PrimaryColor != "" {
			primary = settings.PrimaryColor
		}
		if settings.CompanyName != "" {
			company = html.EscapeString(settings.CompanyName)
		}
	}
	return fmt.Sprintf(`<!DOCTYPE html>
<html>
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"></head>
<body style="margin:0;padding:0;background:#f4f4f2;font-family:system-ui,-apple-system,sans-serif;color:#1a1a1a;">
<table width="100%%" cellpadding="0" cellspacing="0" style="background:#f4f4f2;padding:32px 16px;">
  <tr><td align="center">
    <table width="520" cellpadding="0" cellspacing="0" style="max-width:520px;width:100%%;">
      <tr><td style="background:#fff;padding:18px 32px 0;border-radius:8px 8px 0 0;border:1px solid #e8e8e4;border-bottom:none;">
        <span style="font-size:13px;font-weight:600;color:%s;letter-spacing:0.01em;">%s</span>
      </td></tr>
      <tr><td style="background:#fff;padding:0 32px;border-left:1px solid #e8e8e4;border-right:1px solid #e8e8e4;">
        <div style="height:1px;background:%s;margin-top:12px;opacity:0.25;"></div>
      </td></tr>
      <tr><td style="background:#fff;padding:24px 32px 32px;border-radius:0 0 8px 8px;border:1px solid #e8e8e4;border-top:none;">
        %s
      </td></tr>
      <tr><td style="padding:14px 0;text-align:center;">
        <span style="color:#ccc;font-size:11px;">Sent via %s</span>
      </td></tr>
    </table>
  </td></tr>
</table>
</body>
</html>`, primary, company, primary, bodyHTML, company)
}
