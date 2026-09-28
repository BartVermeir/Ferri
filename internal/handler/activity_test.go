package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BartVermeir/Ferri/internal/activity"
)

// A running upload chunk shows on the dashboard with its file, looked up by
// the tusd upload ID; a finished download leaves nothing behind.
func TestAdminDashboard_ShowsRunningUpload(t *testing.T) {
	cfg := newTestConfig()
	stores := newTestStores(t)
	mgr, root := newTestManager(t)
	r := newAdminRouter(cfg, stores, mgr)
	cookie := adminSessionCookie(t, cfg)
	_, fileID, _, _ := mustCreateActiveTransfer(t, stores, root, "")
	if err := stores.Transfers.SetTUSUploadID(fileID, "tus-running-1"); err != nil {
		t.Fatal(err)
	}
	f, err := stores.Transfers.GetFileByID(fileID)
	if err != nil || f == nil {
		t.Fatalf("file: %v", err)
	}

	up := activity.Default.Start(activity.Info{Kind: activity.Upload, UploadID: "tus-running-1", IP: "203.0.113.9"})
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	up.Done()
	body := rr.Body.String()
	for _, want := range []string{"Uploading now", "In progress", "↑ upload", f.OriginalName, "203.0.113.9"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard misses %q", want)
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/transfers/"+f.TransferID+"/file/"+fileID, nil)
	req.AddCookie(cookie)
	r.ServeHTTP(httptest.NewRecorder(), req)
	if n := len(activity.Default.Snapshot().Running); n != 0 {
		t.Fatalf("%d transfers still registered after the download ended", n)
	}
}

func TestRangeStart(t *testing.T) {
	for h, want := range map[string]int64{"": 0, "bytes=0-": 0, "bytes=1048576-": 1048576, "bytes=10-20": 10, "bytes=-500": 0, "items=5-": 0} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if h != "" {
			r.Header.Set("Range", h)
		}
		if got := rangeStart(r); got != want {
			t.Errorf("rangeStart(%q) = %d, want %d", h, got, want)
		}
	}
}

// A finished download is kept by the OnDone hook and shows in the
// statistics dialog, with the transfer's upload side.
func TestAdminStats_ShowsRecordedDownload(t *testing.T) {
	cfg := newTestConfig()
	stores := newTestStores(t)
	mgr, root := newTestManager(t)
	r := newAdminRouter(cfg, stores, mgr)
	cookie := adminSessionCookie(t, cfg)
	_, fileID, _, content := mustCreateActiveTransfer(t, stores, root, "")
	f, _ := stores.Transfers.GetFileByID(fileID)

	activity.Default.OnDone = StoreDownloadStreams(stores)
	defer func() { activity.Default.OnDone = nil }()
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		return rr
	}
	if rr := get("/admin/transfers/" + f.TransferID + "/file/" + fileID); rr.Body.String() != string(content) {
		t.Fatalf("download body %q", rr.Body.String())
	}
	streams, err := stores.Stats.TransferStreams(f.TransferID)
	if err != nil || len(streams) != 1 || streams[0].Who != "admin" || !streams[0].Complete() {
		t.Fatalf("streams = %+v (%v), want one complete admin download", streams, err)
	}

	rr := get("/admin/transfers/" + f.TransferID + "/stats")
	body := rr.Body.String()
	if rr.Code != http.StatusOK {
		t.Fatalf("stats: status %d", rr.Code)
	}
	for _, want := range []string{"Upload", "Downloads", "1 download(s), 1 complete", "admin", f.OriginalName} {
		if !strings.Contains(body, want) {
			t.Errorf("stats misses %q", want)
		}
	}
	if rr := get("/admin/requests/nope/stats"); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown request stats: status %d, want 404", rr.Code)
	}
}

// Speeds show in Mbps (megabits), like line speeds and speed tests.
func TestFormatRate_Mbps(t *testing.T) {
	for _, c := range []struct {
		bytesPerSec float64
		want        string
	}{
		{0, "—"},
		{50_000, "0.40 Mbps"},
		{375_000, "3.0 Mbps"},
		{587_000_000, "4696 Mbps"},
	} {
		if got := formatRate(c.bytesPerSec); got != c.want {
			t.Errorf("formatRate(%v) = %q, want %q", c.bytesPerSec, got, c.want)
		}
	}
}
