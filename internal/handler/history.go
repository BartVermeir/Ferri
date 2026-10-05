package handler

// Transfer history on /admin/history: first what runs now (the "In progress"
// table of the dashboard), then every download and upload of the transfers
// and requests that are still live, newest first. Expired or deleted items
// are not in it. One line is one action: a person uploading or
// downloading files of one item, all files together.

import (
	"fmt"
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
	What      string // file name, or "600 files"
	Who       string
	IP        string
	Sent      string // "12.0 GB", or "12.0 GB from 4.0 GB" for one resumed part
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
		actions := groupHistory(entries)
		rows := make([]historyRow, 0, len(actions))
		for _, a := range actions {
			rows = append(rows, buildHistoryRow(a))
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

// historyKey: entries with the same key can be one action.
type historyKey struct {
	upload                bool
	transferID, requestID string
	who, ip               string
}

// groupHistory folds the entries (newest first) into actions: the uploads
// or downloads of one person, from one address, of one item, where each
// starts within store.UploadSessionGap after the previous one ended. Each
// action is oldest entry first; the actions come newest first.
func groupHistory(entries []store.HistoryEntry) [][]store.HistoryEntry {
	type action struct {
		entries []store.HistoryEntry
		end     time.Time
	}
	var actions []*action
	last := map[historyKey]*action{}
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		k := historyKey{e.Upload, e.TransferID, e.RequestID, e.Who, e.IP}
		a := last[k]
		if a == nil || e.StartedAt.After(a.end.Add(store.UploadSessionGap)) {
			a = &action{}
			last[k] = a
			actions = append(actions, a)
		}
		a.entries = append(a.entries, e)
		if e.End.After(a.end) {
			a.end = e.End
		}
	}
	list := make([][]store.HistoryEntry, len(actions))
	for i, a := range actions {
		list[len(actions)-1-i] = a.entries
	}
	return list
}

// buildHistoryRow makes one line of an action. Its duration is the time
// something was moving: files that went at the same time count once. It is
// complete when every file in it reached its end.
func buildHistoryRow(action []store.HistoryEntry) historyRow {
	e := action[0]
	var bytes int64
	spans := make([][2]int64, 0, len(action))
	done := map[string]bool{} // file name → reached its end
	for _, x := range action {
		bytes += x.Bytes
		spans = append(spans, [2]int64{x.StartedAt.UnixMilli(), x.StartedAt.Add(x.Duration).UnixMilli()})
		done[x.What] = done[x.What] || x.Complete()
	}
	took := time.Duration(store.Covered(spans)) * time.Millisecond
	row := historyRow{
		StartedAt: e.StartedAt, Upload: e.Upload, Item: "transfer",
		Title: e.Title, What: e.What, Who: e.Who, IP: e.IP,
		Sent:     mail.FormatSize(bytes),
		Duration: formatDuration(took),
		Rate:     formatRate(bytesPerSec(bytes, took)),
		Complete: true,
	}
	for _, ok := range done {
		row.Complete = row.Complete && ok
	}
	if len(done) > 1 {
		row.What = fmt.Sprintf("%d files", len(done))
	}
	if e.RequestID != "" {
		row.Item = "request"
	}
	if len(action) == 1 && e.Offset > 0 {
		row.Sent += " from " + mail.FormatSize(e.Offset)
	}
	return row
}
