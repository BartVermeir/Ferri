package handler

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/BartVermeir/Ferri/internal/config"
	appMiddleware "github.com/BartVermeir/Ferri/internal/middleware"
	"github.com/BartVermeir/Ferri/internal/store"
)

type kickCounter struct{ n int }

func (k *kickCounter) KickMail() { k.n++ }

func remoteLoginConfig() *config.Config {
	cfg := newTestConfig()
	cfg.RemoteLogin.Domains = []string{"example.com"}
	_, internal, _ := net.ParseCIDR("10.0.0.0/8")
	cfg.IPAllowlist = []*net.IPNet{internal}
	return cfg
}

// newLoginRouter wires the sign-in routes and the send and request routes
// the way routes.go does.
func newLoginRouter(cfg *config.Config, stores *store.Stores, kicker MailKicker) http.Handler {
	r := chi.NewRouter()
	r.Use(appMiddleware.InjectSettings(stores.Settings))
	r.Use(appMiddleware.CSRFProtect(cfg.Server.BaseURL))
	r.Post("/login", LoginRequest(cfg, stores, kicker))
	r.Get("/login/{token}", LoginPage(cfg, stores))
	r.Post("/login/{token}", LoginConfirm(cfg, stores))
	r.Post("/logout", Logout(cfg))
	r.Group(func(r chi.Router) {
		r.Use(appMiddleware.IPAllowOrRemote(cfg, Forbidden(cfg)))
		r.Get("/", SendPage(cfg, stores))
		r.Post("/send", SendCreate(cfg, stores))
		r.Post("/request", RequestCreate(cfg, stores))
	})
	return r
}

func loginDo(r http.Handler, method, path string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	req.RemoteAddr = "203.0.113.9:4000"
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "http://example.com")
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

var loginLinkRE = regexp.MustCompile(`http://example\.com/login/([1-9A-Za-z]+)`)

// signInLinks returns the tokens in the queued sign-in mails to addr.
func signInLinks(t *testing.T, stores *store.Stores, addr string) []string {
	t.Helper()
	items, err := stores.Mail.ListRecent(50)
	if err != nil {
		t.Fatal(err)
	}
	var toks []string
	for _, m := range items {
		if m.ToAddress != addr || !strings.HasPrefix(m.Subject, "Your sign-in link") {
			continue
		}
		html := loginLinkRE.FindStringSubmatch(m.BodyHTML)
		text := loginLinkRE.FindStringSubmatch(m.BodyText)
		if html == nil || text == nil || html[1] != text[1] {
			t.Fatalf("sign-in mail without the same link in HTML and text:\n%s", m.BodyText)
		}
		toks = append(toks, text[1])
	}
	return toks
}

// The whole sign-in: ask a link, open it (does not use it), click "Sign in"
// (uses it), and the send page opens from outside the internal network.
func TestLogin_FullFlow(t *testing.T) {
	cfg := remoteLoginConfig()
	stores := newTestStores(t)
	kick := &kickCounter{}
	r := newLoginRouter(cfg, stores, kick)

	if rr := loginDo(r, "GET", "/", nil); rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), `action="/login"`) {
		t.Fatalf("403 page without sign-in form: %d", rr.Code)
	}

	rr := loginDo(r, "POST", "/login", url.Values{"email": {" Alice@Example.com "}})
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Check your mail") {
		t.Fatalf("ask link: %d %s", rr.Code, rr.Body.String())
	}
	if kick.n != 1 {
		t.Fatalf("mail job kicked %d times, want 1", kick.n)
	}
	toks := signInLinks(t, stores, "alice@example.com")
	if len(toks) != 1 {
		t.Fatalf("sign-in mails = %d, want 1", len(toks))
	}
	link := "/login/" + toks[0]

	for i := 0; i < 2; i++ {
		rr = loginDo(r, "GET", link, nil)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "alice@example.com") || len(rr.Result().Cookies()) != 0 {
			t.Fatalf("open link %d: %d, cookies %v", i, rr.Code, rr.Result().Cookies())
		}
	}

	rr = loginDo(r, "POST", link, nil)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/" {
		t.Fatalf("sign in: %d -> %q", rr.Code, rr.Header().Get("Location"))
	}
	cookies := rr.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %v", cookies)
	}
	if rr := loginDo(r, "POST", link, nil); rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), "no longer works") {
		t.Fatalf("link used twice: %d", rr.Code)
	}

	rr = loginDo(r, "GET", "/", nil, cookies[0])
	body := rr.Body.String()
	if rr.Code != http.StatusOK || !strings.Contains(body, "Signed in as") ||
		!strings.Contains(body, `value="alice@example.com" readonly`) {
		t.Fatalf("send page signed in: %d", rr.Code)
	}

	rr = loginDo(r, "POST", "/logout", nil, cookies[0])
	if out := rr.Result().Cookies(); rr.Code != http.StatusSeeOther || len(out) != 1 || out[0].MaxAge >= 0 {
		t.Fatalf("sign out: %d %v", rr.Code, out)
	}
}

// Addresses outside remote_login.domains get no link; more than
// loginLinksPerAddress requests get the same answer without a mail.
func TestLogin_OnlyAllowedDomainsAndLimit(t *testing.T) {
	cfg := remoteLoginConfig()
	stores := newTestStores(t)
	r := newLoginRouter(cfg, stores, nil)

	for _, addr := range []string{"mallory@example.org", "alice@mail.example.com", "not-an-address"} {
		if rr := loginDo(r, "POST", "/login", url.Values{"email": {addr}}); rr.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", addr, rr.Code)
		}
	}
	if n, _ := stores.Mail.ListRecent(50); len(n) != 0 {
		t.Fatalf("mails for refused addresses: %d", len(n))
	}

	for i := 0; i < loginLinksPerAddress+2; i++ {
		if rr := loginDo(r, "POST", "/login", url.Values{"email": {"alice@example.com"}}); rr.Code != http.StatusOK {
			t.Fatalf("request %d: %d", i, rr.Code)
		}
	}
	if toks := signInLinks(t, stores, "alice@example.com"); len(toks) != loginLinksPerAddress {
		t.Fatalf("sign-in mails = %d, want %d", len(toks), loginLinksPerAddress)
	}
}

// A signed-in colleague sends and requests as the signed-in address,
// whatever the form says.
func TestLogin_SenderIsTheSignedInAddress(t *testing.T) {
	cfg := remoteLoginConfig()
	d := newTestDB(t)
	stores := store.New(d)
	r := newLoginRouter(cfg, stores, nil)
	rr := httptest.NewRecorder()
	appMiddleware.SetRemoteCookie(rr, cfg, "alice@example.com")
	cookie := rr.Result().Cookies()[0]

	rr = loginDo(r, "POST", "/send", url.Values{
		"sender_email": {"ceo@example.com"},
		"recipients":   {"bob@example.org"},
		"expiry_hours": {"24"},
		"files":        {`[{"name":"a.mov","size":1}]`},
	}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("send: %d %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		TransferID string `json:"transfer_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	tr, err := stores.Transfers.GetByID(resp.TransferID)
	if err != nil || tr == nil {
		t.Fatalf("transfer: %v", err)
	}
	if tr.SenderEmail != "alice@example.com" {
		t.Fatalf("sender = %q, want the signed-in address", tr.SenderEmail)
	}

	rr = loginDo(r, "POST", "/request", url.Values{
		"requester_email": {"ceo@example.com"},
		"title":           {"Rushes"},
		"expiry_hours":    {"24"},
	}, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("request: %d", rr.Code)
	}
	var requester string
	if err := d.QueryRow(`SELECT requester_email FROM upload_requests`).Scan(&requester); err != nil {
		t.Fatal(err)
	}
	if requester != "alice@example.com" {
		t.Fatalf("requester = %q, want the signed-in address", requester)
	}
}

// With remote_login off, the 403 page has no form and the routes are 404.
func TestLogin_Off(t *testing.T) {
	cfg := newTestConfig()
	stores := newTestStores(t)
	r := newLoginRouter(cfg, stores, nil)
	if rr := loginDo(r, "GET", "/", nil); strings.Contains(rr.Body.String(), `action="/login"`) {
		t.Fatal("sign-in form while remote login is off")
	}
	if rr := loginDo(r, "POST", "/login", url.Values{"email": {"alice@example.com"}}); rr.Code != http.StatusNotFound {
		t.Fatalf("POST /login = %d, want 404", rr.Code)
	}
}

// The 403 page under /admin has no sign-in form: signing in does not open it.
func TestForbiddenPage_NoSignInUnderAdmin(t *testing.T) {
	cfg := remoteLoginConfig()
	stores := newTestStores(t)
	h := appMiddleware.InjectSettings(stores.Settings)(Forbidden(cfg))
	for path, want := range map[string]bool{"/": true, "/manage/x": true, "/admin": false, "/admin/login": false} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if got := strings.Contains(rr.Body.String(), `action="/login"`); got != want {
			t.Errorf("%s: sign-in form %v, want %v", path, got, want)
		}
	}
}

// With a no-referrer policy the browser sends "Origin: null" on a form POST,
// which CSRFProtect refuses. No page may set one.
func TestTemplates_NoReferrerPolicy(t *testing.T) {
	for _, tpl := range templates.Templates() {
		if tpl.Tree != nil && strings.Contains(tpl.Tree.Root.String(), "no-referrer") {
			t.Errorf("template %s sets a no-referrer policy", tpl.Name())
		}
	}
}
