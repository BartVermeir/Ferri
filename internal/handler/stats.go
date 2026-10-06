package handler

// Statistics of one transfer or request for the admin,
// shown in a dialog on the dashboard: admin-stats.js fetches the fragment
// from GET /admin/transfers/{id}/stats or /admin/requests/{id}/stats.

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/BartVermeir/Ferri/internal/config"
	"github.com/BartVermeir/Ferri/internal/mail"
	"github.com/BartVermeir/Ferri/internal/storage"
	"github.com/BartVermeir/Ferri/internal/store"
)

type statsView struct {
	Kind      string // "transfer" or "request"
	Title     string
	Status    string
	CreatedAt time.Time
	ExpiresAt time.Time

	Files       int
	Size        string
	UploadStart time.Time // zero = no complete files
	UploadEnd   time.Time
	Elapsed     string // start to last chunk, pauses included
	ElapsedRate string
	Net         string // time files were uploading, overlap once; "" = not known (a complete file without upload sessions)
	NetRate     string
	From        string // upload IPs, comma-separated; "" = not known

	Streams   []streamRow
	Complete  int // streams that reached the end
	BytesSent string

	Integrity []integrityRow // files of a transfer sent with "Check upload integrity"
}

// integrityRow is one file's upload integrity check (storage/diag.go).
type integrityRow struct {
	Name        string
	Browser     string // User-Agent that created the upload; "" = not known
	Size        string
	TUSID       string // /admin/diag/{TUSID} gives the blocks
	State       string
	Blocks      int64
	NotMeasured int
	Differ      int
	Identical   bool // verified, every block measured, none differs, stored size equal
}

type streamRow struct {
	StartedAt time.Time
	Who       string
	What      string
	IP        string
	Duration  string
	Sent      string // "12.0 GB" or "12.0 GB from 4.0 GB" for a resumed part
	Rate      string
	Complete  bool
}

// AdminTransferStats handles GET /admin/transfers/{id}/stats.
func AdminTransferStats(cfg *config.Config, stores *store.Stores, mgr *storage.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t, err := stores.Transfers.GetByID(chi.URLParam(r, "id"))
		if err != nil || t == nil {
			http.NotFound(w, r)
			return
		}
		up, err := stores.Stats.TransferUploadStats(t.ID)
		if err != nil {
			slog.Error("admin stats: upload", "transfer_id", t.ID, "error", err)
		}
		streams, err := stores.Stats.TransferStreams(t.ID)
		if err != nil {
			slog.Error("admin stats: streams", "transfer_id", t.ID, "error", err)
		}
		v := buildStatsView(up, streams)
		v.Kind, v.Title, v.Status, v.CreatedAt, v.ExpiresAt = "transfer", t.Title, t.Status, t.CreatedAt, t.ExpiresAt
		if mgr.DiagEnabled() {
			files, err := stores.Transfers.GetFilesByTransferID(t.ID)
			if err != nil {
				slog.Error("admin stats: files", "transfer_id", t.ID, "error", err)
			}
			v.Integrity = buildIntegrity(mgr, files)
		}
		renderPage(w, "admin/stats.html", v)
	}
}

// AdminRequestStats handles GET /admin/requests/{id}/stats.
func AdminRequestStats(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, err := stores.Requests.GetByID(chi.URLParam(r, "id"))
		if err != nil || req == nil {
			http.NotFound(w, r)
			return
		}
		up, err := stores.Stats.RequestUploadStats(req.ID)
		if err != nil {
			slog.Error("admin stats: upload", "request_id", req.ID, "error", err)
		}
		streams, err := stores.Stats.RequestStreams(req.ID)
		if err != nil {
			slog.Error("admin stats: streams", "request_id", req.ID, "error", err)
		}
		v := buildStatsView(up, streams)
		v.Kind, v.Title, v.Status, v.CreatedAt, v.ExpiresAt = "request", req.Title, req.Status, req.CreatedAt, req.ExpiresAt
		renderPage(w, "admin/stats.html", v)
	}
}

func buildStatsView(up store.UploadStats, streams []store.DownloadStream) statsView {
	v := statsView{Files: up.Files, Size: mail.FormatSize(up.Bytes)}
	if up.Files > 0 {
		v.UploadStart, v.UploadEnd = up.Start, up.End
		elapsed := up.End.Sub(up.Start)
		v.Elapsed = formatDuration(elapsed)
		v.ElapsedRate = formatRate(bytesPerSec(up.Bytes, elapsed))
		v.From = strings.Join(up.IPs, ", ")
		if up.NetKnown {
			v.Net = formatDuration(up.Net)
			v.NetRate = formatRate(bytesPerSec(up.Bytes, up.Net))
		}
	}
	var sent int64
	for _, d := range streams {
		row := streamRow{
			StartedAt: d.StartedAt, Who: d.Who, What: d.What, IP: d.IP,
			Duration: formatDuration(d.Duration),
			Sent:     mail.FormatSize(d.Bytes),
			Rate:     formatRate(bytesPerSec(d.Bytes, d.Duration)),
			Complete: d.Complete(),
		}
		if d.Offset > 0 {
			row.Sent += " from " + mail.FormatSize(d.Offset)
		}
		if row.Complete {
			v.Complete++
		}
		sent += d.Bytes
		v.Streams = append(v.Streams, row)
	}
	v.BytesSent = mail.FormatSize(sent)
	return v
}

// buildIntegrity lists the files that have an integrity check record, in the
// order of files.
func buildIntegrity(mgr *storage.Manager, files []store.File) []integrityRow {
	var rows []integrityRow
	for _, f := range files {
		if !f.TUSUploadID.Valid {
			continue
		}
		d, ok := mgr.DiagUploadByID(f.TUSUploadID.String)
		if !ok {
			continue
		}
		rows = append(rows, integrityRow{
			Name: f.OriginalName, Browser: d.UserAgent, Size: mail.FormatSize(d.Size), TUSID: d.ID, State: d.State,
			Blocks: d.Blocks, NotMeasured: d.NotMeasured, Differ: d.Differ,
			Identical: d.State == storage.DiagStateVerified && d.NotMeasured == 0 && d.Differ == 0 && d.StoredSize == d.Size,
		})
	}
	return rows
}

// bytesPerSec is 0 under a second: too short to say anything.
func bytesPerSec(b int64, d time.Duration) float64 {
	if d < time.Second {
		return 0
	}
	return float64(b) / d.Seconds()
}

// formatDuration is formatRunning, but under a minute with one decimal:
// a small file's upload takes a second or two.
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return d.Round(100 * time.Millisecond).String()
	}
	return formatRunning(d)
}
