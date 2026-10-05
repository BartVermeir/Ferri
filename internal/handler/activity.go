package handler

// The "Now" block on the admin dashboard: what is being
// uploaded and downloaded at this moment, from activity.Default. Read once
// per page load, no live updates.

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/BartVermeir/Ferri/internal/activity"
	"github.com/BartVermeir/Ferri/internal/mail"
	"github.com/BartVermeir/Ferri/internal/store"
)

type activityRow struct {
	Kind     string // activity.Upload or activity.Download
	Item     string // "transfer", "request", "" when unknown
	Title    string
	File     string
	Who      string
	IP       string
	Running  string // "12 min"
	Progress string // "15.9 GB of 36.0 GB"
	Rate     string // "1456 Mbps", "—" with less than a second to go on
	// Remaining is an estimate: what is left at the current speed ("~12 min").
	// "—" without a known size or speed.
	Remaining string
}

type activityView struct {
	Uploads, Downloads       int
	UploadRate, DownloadRate string
	// Open connections from the reverse proxy, without the one serving this
	// page; Idle of them wait for a next request (keep-alive). This is what
	// scripts/deploy.sh counts.
	OpenConns, IdleConns int
	Rows                 []activityRow
}

func buildActivityView(stores *store.Stores, snap activity.Snapshot) activityView {
	v := activityView{OpenConns: max(snap.OpenConns-1, 0), IdleConns: snap.IdleConns}
	var upRate, downRate float64
	for _, x := range snap.Running {
		rate := x.Rate(snap.Taken)
		row := activityRow{
			Kind: x.Kind, Item: x.Item, Title: x.Title, File: x.File, Who: x.Who, IP: x.IP,
			Running: formatRunning(snap.Taken.Sub(x.Started)),
			Rate:    formatRate(rate),
		}
		total := x.Total
		if x.Kind == activity.Upload {
			v.Uploads++
			upRate += rate
			// An upload is registered per chunk; its file and how long the
			// upload of that file has been going come from the database.
			if l := uploadLabel(stores, x.UploadID); l != nil {
				row.Title, row.File, row.Who, total = l.Title, l.FileName, l.Who, l.Size
				row.Item = "transfer"
				if l.Who == "uploader" {
					row.Item = "request"
				}
				row.Running = formatRunning(snap.Taken.Sub(l.CreatedAt))
			} else {
				row.File = "(unknown upload " + x.UploadID + ")"
			}
		} else {
			v.Downloads++
			downRate += rate
		}
		row.Progress = mail.FormatSize(x.Position())
		if total > 0 {
			row.Progress += " of " + mail.FormatSize(total)
		}
		row.Remaining = estimateRemaining(total-x.Position(), rate)
		v.Rows = append(v.Rows, row)
	}
	v.UploadRate, v.DownloadRate = formatRate(upRate), formatRate(downRate)
	return v
}

// StoreStreams is the activity.Registry.OnDone hook that keeps every
// finished download for the statistics and the transfer history, and every
// upload chunk for the transfer history. Something that moved no bytes (a
// 304, a refused Range, an empty PATCH) is not kept.
func StoreStreams(stores *store.Stores) func(activity.Running, time.Duration) {
	return func(x activity.Running, took time.Duration) {
		if x.Bytes == 0 {
			return
		}
		if x.Kind == activity.Upload {
			storeUploadChunk(stores, x, took)
			return
		}
		d := store.DownloadStream{
			Who: x.Who, What: x.File, IP: x.IP,
			Offset: x.Offset, Bytes: x.Bytes, Total: x.Total,
			StartedAt: x.Started, Duration: took,
		}
		if x.Item == "request" {
			d.RequestID = x.ItemID
		} else {
			d.TransferID = x.ItemID
		}
		if err := stores.Stats.RecordStream(d); err != nil {
			slog.Error("stats: record download stream", "item", x.ItemID, "error", err)
		}
	}
}

// storeUploadChunk: a chunk only knows its tusd upload ID; the file and who
// uploads it come from the database, like on the dashboard.
func storeUploadChunk(stores *store.Stores, x activity.Running, took time.Duration) {
	l := uploadLabel(stores, x.UploadID)
	if l == nil {
		return
	}
	c := store.UploadChunk{
		TUSUploadID: x.UploadID, Who: l.Who, What: l.FileName, IP: x.IP,
		Offset: x.Offset, Bytes: x.Bytes, Total: l.Size,
		StartedAt: x.Started, Duration: took,
	}
	if l.Who == "uploader" {
		c.RequestID = l.ItemID
	} else {
		c.TransferID = l.ItemID
	}
	if err := stores.Stats.RecordUploadChunk(c); err != nil {
		slog.Error("stats: record upload chunk", "item", l.ItemID, "error", err)
	}
}

func uploadLabel(stores *store.Stores, tusID string) *store.UploadLabel {
	l, err := stores.Transfers.UploadLabelByTUSID(tusID)
	if err == nil && l == nil {
		l, err = stores.Requests.UploadLabelByTUSID(tusID)
	}
	if err != nil {
		slog.Error("admin: look up running upload", "tus_upload_id", tusID, "error", err)
		return nil
	}
	return l
}

// formatRate shows a speed in Mbps (megabits, 1 Mbps = 125,000 bytes per
// second).
func formatRate(bytesPerSec float64) string {
	if bytesPerSec <= 0 {
		return "—"
	}
	mbps := bytesPerSec * 8 / 1e6
	switch {
	case mbps < 1:
		return fmt.Sprintf("%.2f Mbps", mbps)
	case mbps < 10:
		return fmt.Sprintf("%.1f Mbps", mbps)
	}
	return fmt.Sprintf("%.0f Mbps", mbps)
}

// estimateRemaining is left / speed, rounded like formatRunning. A ZIP is a
// little larger than its files, so it can reach 0 before the end.
func estimateRemaining(left int64, bytesPerSec float64) string {
	if left <= 0 || bytesPerSec <= 0 {
		return "—"
	}
	return "~" + formatRunning(time.Duration(float64(left)/bytesPerSec*float64(time.Second)))
}

func formatRunning(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	default:
		return fmt.Sprintf("%d h %02d min", int(d.Hours()), int(d.Minutes())%60)
	}
}
