package handler

// Transfer history on /admin/history: first what runs now (the "In progress"
// table of the dashboard), then every download and upload of the transfers
// and requests that are still live, newest first. Expired or deleted items
// are gone from it.

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/BartVermeir/Ferri/internal/activity"
	"github.com/BartVermeir/Ferri/internal/config"
	"github.com/BartVermeir/Ferri/internal/mail"
	appMiddleware "github.com/BartVermeir/Ferri/internal/middleware"
	"github.com/BartVermeir/Ferri/internal/store"
)

type historyRow struct {
	StartedAt time.Time
	Upload    bool
	Item      string // "transfer" or "request"
	Title     string
	What      string
	Who       string
	IP        string
	Sent      string // "12.0 GB" or "12.0 GB from 4.0 GB" for a resumed part
	Duration  string
	Rate      string
	Complete  bool
}

// AdminHistory handles GET /admin/history.
func AdminHistory(cfg *config.Config, stores *store.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		entries, err := stores.Stats.History()
		if err != nil {
			slog.Error("admin history: list", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		rows := make([]historyRow, 0, len(entries))
		for _, e := range entries {
			rows = append(rows, buildHistoryRow(e))
		}
		renderPage(w, "admin/history.html", struct {
			adminData
			Now  activityView
			Rows []historyRow
		}{
			adminData: adminData{PageTitle: "Transfer history", ActiveNav: "history", Settings: appMiddleware.GetSettings(r)},
			Now:       buildActivityView(stores, activity.Default.Snapshot()),
			Rows:      rows,
		})
	}
}

func buildHistoryRow(e store.HistoryEntry) historyRow {
	row := historyRow{
		StartedAt: e.StartedAt, Upload: e.Upload, Item: "transfer",
		Title: e.Title, What: e.What, Who: e.Who, IP: e.IP,
		Sent:     mail.FormatSize(e.Bytes),
		Duration: formatDuration(e.Duration),
		Rate:     formatRate(bytesPerSec(e.Bytes, e.Duration)),
		Complete: e.Complete(),
	}
	if e.RequestID != "" {
		row.Item = "request"
	}
	if e.Offset > 0 {
		row.Sent += " from " + mail.FormatSize(e.Offset)
	}
	return row
}
