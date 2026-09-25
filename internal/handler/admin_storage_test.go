package handler

// Audit M4: the saved SMB password may only be reused (empty password field)
// for the saved host, share, username and domain. Otherwise an admin session
// could point Ferri at any server and capture the service account's login.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/BartVermeir/Ferri/internal/storage"
	"github.com/BartVermeir/Ferri/internal/store"
)

func savedSMBStores(t *testing.T, adminToken string) *store.Stores {
	t.Helper()
	stores := newTestStores(t)
	enc, err := storage.Encrypt(adminToken, "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"storage.type": "smb", "storage.smb_host": "nas.example.test", "storage.smb_share": "ferri",
		"storage.smb_username": "svc-ferri", "storage.smb_domain": "CORP",
		"storage.smb_password_encrypted": enc,
	} {
		if err := stores.Settings.Save(k, v); err != nil {
			t.Fatal(err)
		}
	}
	return stores
}

func smbForm(host string) url.Values {
	return url.Values{
		"storage.type": {"smb"}, "storage.smb_host": {host}, "storage.smb_share": {"ferri"},
		"storage.smb_username": {"svc-ferri"}, "storage.smb_domain": {"CORP"}, "storage.smb_password": {""},
	}
}

func postForm(h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// "Test connection" with another host and an empty password must not log in
// there with the saved password. The host is an unusable address so that the
// old code fails fast with a dial error instead of our refusal.
func TestAdminStorageTest_OtherHostNeedsPassword(t *testing.T) {
	cfg := newTestConfig()
	stores := savedSMBStores(t, cfg.Admin.Token)

	rr := postForm(AdminStorageTest(cfg, stores), "/admin/settings/storage/test", smbForm("127.0.0.1:1"))
	var got struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("response %q: %v", rr.Body.String(), err)
	}
	if got.OK || got.Error != msgSMBPasswordAgain {
		t.Fatalf("got %+v, want a refusal asking for the password", got)
	}
}

// Saving another host with an empty password is refused and changes nothing:
// saving reloads the backend, which would log in with the saved password.
func TestAdminStorageSave_OtherHostNeedsPassword(t *testing.T) {
	cfg := newTestConfig()
	stores := savedSMBStores(t, cfg.Admin.Token)

	rr := postForm(AdminStorageSave(cfg, stores, mustManager(t)), "/admin/settings/storage", smbForm("127.0.0.1:1"))
	if rr.Code != http.StatusSeeOther || !strings.Contains(rr.Header().Get("Location"), "storage_error=") {
		t.Fatalf("status = %d, location = %q, want a redirect with storage_error", rr.Code, rr.Header().Get("Location"))
	}
	if host := stores.Settings.Get().SMBHost; host != "nas.example.test" {
		t.Fatalf("saved host = %q, want unchanged", host)
	}
}

func TestMayReuseSMBPassword(t *testing.T) {
	saved := &store.Settings{SMBHost: "nas.example.test", SMBShare: "ferri", SMBUsername: "svc-ferri",
		SMBDomain: "CORP", SMBPasswordEncrypted: "x"}
	req := func(f url.Values) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(f.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return r
	}
	same := smbForm("NAS.example.test ")
	same.Set("storage.smb_domain", "corp")
	otherUser := smbForm("nas.example.test")
	otherUser.Set("storage.smb_username", "someone")

	cases := []struct {
		name  string
		saved *store.Settings
		form  url.Values
		want  bool
	}{
		{"same target, host and domain in other case", saved, same, true},
		{"other host", saved, smbForm("evil.example.test"), false},
		{"other user", saved, otherUser, false},
		{"nothing saved to leak", &store.Settings{}, smbForm("evil.example.test"), true},
	}
	for _, c := range cases {
		if got := mayReuseSMBPassword(c.saved, req(c.form)); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
