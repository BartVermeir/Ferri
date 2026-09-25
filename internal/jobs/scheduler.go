package jobs

import (
	"fmt"
	"html"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/BartVermeir/Ferri/internal/config"
	"github.com/BartVermeir/Ferri/internal/mail"
	"github.com/BartVermeir/Ferri/internal/storage"
	"github.com/BartVermeir/Ferri/internal/store"
)

// Scheduler runs background jobs on configurable intervals.
type Scheduler struct {
	cfg       *config.Config
	stores    *store.Stores
	mgr       *storage.Manager
	stop      chan struct{}
	wg        sync.WaitGroup
	startOnce sync.Once // guards against Start() being called more than once
	// cleanupMu keeps one cleanup run at a time: "Force cleanup" in the
	// admin could otherwise overlap the scheduled run (audit L9).
	cleanupMu sync.Mutex
}

// NewScheduler creates a Scheduler. Call Start() to begin running jobs.
func NewScheduler(cfg *config.Config, stores *store.Stores, mgr *storage.Manager) *Scheduler {
	return &Scheduler{
		cfg:    cfg,
		stores: stores,
		mgr:    mgr,
		stop:   make(chan struct{}),
	}
}

// Start launches all background jobs in separate goroutines.
// It is safe to call only once; subsequent calls are no-ops.
func (s *Scheduler) Start() {
	s.startOnce.Do(func() {
		s.wg.Add(3)
		go s.runLoop("mail", time.Duration(s.cfg.Jobs.MailIntervalMinutes)*time.Minute, s.runMailJob)
		go s.runLoop("expiry", time.Duration(s.cfg.Jobs.ExpiryIntervalMinutes)*time.Minute, s.runExpiryJob)
		go s.runLoop("cleanup", time.Duration(s.cfg.Jobs.CleanupIntervalHours)*time.Hour, func() { s.runCleanupJob() })
	})
}

// Stop signals all jobs to stop and waits for them to finish their current run.
// This ensures in-progress cleanup or mail jobs complete before the process exits.
func (s *Scheduler) Stop() {
	close(s.stop)
	s.wg.Wait()
}

func (s *Scheduler) runLoop(name string, interval time.Duration, fn func()) {
	defer s.wg.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			runJob(name, fn)
		}
	}
}

// runJob executes one job tick, recovering from a panic so a single bad run is
// logged instead of taking down the process. The container entrypoint is
// `litestream replicate -exec /ferri`, so an unrecovered job panic exits the
// whole container and Docker restarts it straight into the same panic on the
// next tick.
func runJob(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("job panic recovered", "job", name, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	fn()
}

// ── Mail job ──────────────────────────────────────────────────────────────────

// Mail job batches: mailBatchSize per fetch, and while a batch comes back
// full, up to mailBatchesPerRun of them in one run: at most 100 mails per
// run (every 2 minutes by default), all over one SMTP connection (audit O5).
const (
	mailBatchSize     = 20
	mailBatchesPerRun = 5
)

// runMailJob sends pending mails, see the batch constants above.
// Sends via SMTP and updates status. Retries on failure with exponential backoff.
// See architecture.md §10 (Mail job) for the full specification.
func (s *Scheduler) runMailJob() {
	// Load settings once per run — provides runtime from_address and from_name.
	sender := mail.NewSender(s.cfg, s.stores.Settings.Get())
	defer sender.Close()

	for batch := 0; batch < mailBatchesPerRun; batch++ {
		items, err := s.stores.Mail.FetchPending(mailBatchSize)
		if err != nil {
			slog.Error("mail job: fetch pending", "error", err)
			break
		}
		s.sendBatch(sender, items)
		if len(items) < mailBatchSize {
			break
		}
	}

	// Prune old sent mails
	n, err := s.stores.Mail.PruneSent(s.cfg.Jobs.MailRetentionDays)
	if err != nil {
		slog.Error("mail job: prune sent", "error", err)
	} else if n > 0 {
		slog.Info("mail job: pruned sent mails", "count", n)
	}
}

func (s *Scheduler) sendBatch(sender *mail.Sender, items []store.MailItem) {
	for _, item := range items {
		if err := sender.Send(item); err != nil {
			slog.Warn("mail job: send failed",
				"id", item.ID, "to", item.ToAddress,
				"attempt", item.Attempts+1, "error", err)
			if err := s.stores.Mail.MarkFailed(item.ID, err.Error()); err != nil {
				slog.Error("mail job: mark failed", "id", item.ID, "error", err)
			}
		} else {
			slog.Info("mail sent", "id", item.ID, "to", item.ToAddress)
			if err := s.stores.Mail.MarkSent(item.ID); err != nil {
				slog.Error("mail job: mark sent", "id", item.ID, "error", err)
			}
		}
	}
}

// ── Expiry job ────────────────────────────────────────────────────────────────

// runExpiryJob finds expired transfers and upload requests, marks them expired,
// and enqueues expiry summary mails.
func (s *Scheduler) runExpiryJob() {
	// Expire transfers
	transfers, err := s.stores.Transfers.GetExpired()
	if err != nil {
		slog.Error("expiry job: get expired transfers", "error", err)
		return
	}

	for _, t := range transfers {
		if err := s.stores.Transfers.SetExpired(t.ID); err != nil {
			slog.Error("expiry job: set expired", "transfer", t.ID, "error", err)
			continue
		}

		// Build and enqueue expiry summary mail if enabled and from-address is configured.
		// Not for a transfer that never went live (upload abandoned while
		// pending): nobody was sent a link, so "never opened" would mislead.
		settings := s.stores.Settings.Get()
		if t.ActivatedAt.Valid && settings.ExpirySummary && settings.MailFromAddress != "" {
			if err := s.enqueueSummary(t, time.Time{}); err != nil {
				slog.Error("expiry job: enqueue summary", "transfer", t.ID, "error", err)
			}
		}
	}

	// Expire upload requests
	requests, err := s.stores.Requests.GetExpired()
	if err != nil {
		slog.Error("expiry job: get expired requests", "error", err)
		return
	}

	for _, r := range requests {
		if err := s.stores.Requests.SetExpired(r.ID); err != nil {
			slog.Error("expiry job: set request expired", "request", r.ID, "error", err)
		}
	}

	if total := len(transfers) + len(requests); total > 0 {
		slog.Info("expiry job: expired", "transfers", len(transfers), "requests", len(requests))
	}
}

// EnqueueDeletionSummary sends the sender the "who downloaded what" summary
// when an admin deletes a transfer by hand. Call it before deleting: the
// file list is empty afterwards. Only for a transfer that is live now: an
// expired one already had its summary, a pending one never reached anyone.
// Same switch as the expiry summary. Returns whether a mail was queued.
func (s *Scheduler) EnqueueDeletionSummary(transferID string) (bool, error) {
	t, err := s.stores.Transfers.GetByID(transferID)
	if err != nil || t == nil {
		return false, err
	}
	settings := s.stores.Settings.Get()
	if t.Status != "active" || !t.ActivatedAt.Valid || !settings.ExpirySummary || settings.MailFromAddress == "" {
		return false, nil
	}
	if err := s.enqueueSummary(*t, time.Now()); err != nil {
		return false, err
	}
	return true, nil
}

// enqueueSummary builds and enqueues the summary mail for a transfer that
// expired (deletedAt zero) or that an admin deleted at deletedAt.
func (s *Scheduler) enqueueSummary(t store.Transfer, deletedAt time.Time) error {
	history, err := s.stores.Downloads.GetHistoryForTransfer(t.ID)
	if err != nil {
		return err
	}
	dbFiles, err := s.stores.Transfers.GetFilesByTransferID(t.ID)
	if err != nil {
		return err
	}

	loc := s.cfg.Server.Location
	if loc == nil {
		loc = time.UTC
	}

	e := expirySummary{
		Settings:   s.stores.Settings.Get(),
		BaseURL:    s.cfg.Server.BaseURL,
		Loc:        loc,
		Transfer:   t,
		History:    history,
		GraceHours: s.cfg.Jobs.CleanupGraceHours,
		DeletedAt:  deletedAt,
	}
	// Only complete files: a dead 'uploading' row (a lost TUS create) was
	// never part of the transfer the recipients saw.
	for _, f := range dbFiles {
		if f.Status == "complete" {
			e.Files = append(e.Files, f)
		}
	}

	title := "your transfer"
	if t.Title != "" {
		title = `"` + t.Title + `"`
	}
	subject := "Expired: " + title
	if !deletedAt.IsZero() {
		subject = "Deleted: " + title
	}
	return s.stores.Mail.Enqueue(nil, t.SenderEmail, subject, buildExpirySummaryHTML(e), buildExpirySummaryText(e))
}

// ── Cleanup job ───────────────────────────────────────────────────────────────

// RunCleanupNow runs the cleanup job immediately, bypassing the grace period.
// Called by the admin "Force cleanup" button.
func (s *Scheduler) RunCleanupNow() {
	s.runCleanupJob(0)
}

// runCleanupJob deletes files from storage for expired transfers past the grace period,
// and cleans up stalled uploads.
func (s *Scheduler) runCleanupJob(graceHours ...int) {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()

	grace := s.cfg.Jobs.CleanupGraceHours
	if len(graceHours) > 0 {
		grace = graceHours[0]
	}

	var totalBytes int64
	var totalTransfers int

	// Clean up expired transfers
	transfers, err := s.stores.Transfers.GetForCleanup(grace)
	if err != nil {
		slog.Error("cleanup job: get for cleanup", "error", err)
		return
	}
	slog.Info("cleanup job: found transfers to clean", "count", len(transfers))

	for _, t := range transfers {
		size, err := s.stores.Transfers.SumFileSizes(t.ID)
		if err != nil {
			slog.Error("cleanup job: sum sizes", "transfer", t.ID, "error", err)
		}

		// Remove flat TUS files (the actual content) — files are stored as
		// <tus_upload_id> and <tus_upload_id>.info, not in a subdirectory.
		files, err := s.stores.Transfers.GetFilesByTransferID(t.ID)
		if err != nil {
			slog.Error("cleanup job: get files", "transfer", t.ID, "error", err)
		} else {
			for _, f := range files {
				if !f.TUSUploadID.Valid {
					slog.Warn("cleanup job: file has no tus_upload_id, may be orphaned on storage",
						"transfer", t.ID, "file", f.ID, "storage_path", f.StoragePath)
				}
				s.removeAndPurge(store.TransferFiles, f.ID, f.StoragePath, f.TUSUploadID.String)
			}
		}
		// Best-effort removal of (empty) transfer directory
		if err := s.mgr.RemoveAll("transfers/" + t.ID); err != nil {
			slog.Warn("cleanup job: remove transfer dir", "transfer", t.ID, "error", err)
		}

		if err := s.stores.Transfers.MarkFilesDeleted(t.ID); err != nil {
			slog.Error("cleanup job: mark deleted", "transfer", t.ID, "error", err)
			continue
		}
		if err := s.stores.Transfers.SoftDelete(t.ID); err != nil {
			slog.Error("cleanup job: soft delete transfer", "transfer", t.ID, "error", err)
		}

		totalBytes += size
		totalTransfers++
	}

	// Clean up expired upload requests
	requests, err := s.stores.Requests.GetForCleanup(grace)
	if err != nil {
		slog.Error("cleanup job: get requests for cleanup", "error", err)
	}
	slog.Info("cleanup job: found requests to clean", "count", len(requests))

	for _, r := range requests {
		slog.Info("cleanup job: cleaning request", "id", r.ID, "status", r.Status, "expires_at", r.ExpiresAt, "expired_at", r.ExpiredAt)
		files, err := s.stores.Requests.GetFiles(r.ID)
		if err != nil {
			slog.Error("cleanup job: get request files", "request", r.ID, "error", err)
		} else {
			for _, f := range files {
				if !f.TUSUploadID.Valid {
					slog.Warn("cleanup job: request file has no tus_upload_id, may be orphaned on storage",
						"request", r.ID, "file", f.ID, "storage_path", f.StoragePath)
				}
				s.removeAndPurge(store.RequestFiles, f.ID, f.StoragePath, f.TUSUploadID.String)
			}
		}
		if err := s.mgr.RemoveAll("requests/" + r.ID); err != nil {
			slog.Warn("cleanup job: remove request dir", "request", r.ID, "error", err)
		}
		if err := s.stores.Requests.MarkFilesDeleted(r.ID); err != nil {
			slog.Error("cleanup job: mark request files deleted", "request", r.ID, "error", err)
		}
		if err := s.stores.Requests.SoftDelete(r.ID); err != nil {
			slog.Error("cleanup job: soft delete request", "request", r.ID, "error", err)
		}
	}

	// Clean up stalled uploads (independent of transfer expiry)
	s.cleanupStalled()

	// Retry every deleted file whose data is not confirmed gone yet.
	s.purgeLeftovers()

	if totalTransfers > 0 {
		slog.Info("cleanup job: complete",
			"transfers", totalTransfers,
			"gb_freed", float64(totalBytes)/1_073_741_824,
		)
	}
}

// cleanupStalled removes partial TUS uploads that have been idle for stall_timeout_hours.
// COALESCE on tus_last_activity_at catches uploads that never received a chunk.
func (s *Scheduler) cleanupStalled() {
	stalledFiles, err := s.stores.Transfers.GetStalled(s.cfg.Jobs.StallTimeoutHours)
	if err != nil {
		slog.Error("cleanup job: get stalled files", "error", err)
		return
	}

	var removed int
	for _, f := range stalledFiles {
		// The data lives at the flat <tus_upload_id>, not at storage_path — this
		// used to remove only storage_path, leaking every stalled upload.
		// Both content and .info go: with only the content gone, tusd would
		// still offer the upload for resumption (DEC-028).
		s.removeAndPurge(store.TransferFiles, f.ID, f.StoragePath, f.TUSUploadID.String)

		if err := s.stores.Transfers.MarkFileDeleted(f.ID); err != nil {
			slog.Error("cleanup job: mark stalled deleted", "file", f.ID, "error", err)
			continue
		}
		removed++
	}

	// Same for upload_request_files — count only successes
	stalledReqFiles, err := s.stores.Requests.GetStalled(s.cfg.Jobs.StallTimeoutHours)
	if err != nil {
		slog.Error("cleanup job: get stalled request files", "error", err)
		return
	}
	var removedReq int
	for _, f := range stalledReqFiles {
		s.removeAndPurge(store.RequestFiles, f.ID, f.StoragePath, f.TUSUploadID.String)
		if err := s.stores.Requests.MarkFileDeleted(f.ID); err != nil {
			slog.Error("cleanup job: mark stalled request file deleted", "file", f.ID, "error", err)
			continue
		}
		removedReq++
	}

	if total := removed + removedReq; total > 0 {
		slog.Info("cleanup job: stalled uploads removed", "count", total)
	}
}

// removeAndPurge wraps storage.PurgeUpload with logging; a failure is retried
// by purgeLeftovers on every later cleanup run. Returns whether the data is gone.
func (s *Scheduler) removeAndPurge(table store.FileTable, fileID, storagePath, tusUploadID string) bool {
	if err := storage.PurgeUpload(s.mgr, s.stores.Files, table, fileID, storagePath, tusUploadID); err != nil {
		slog.Warn("cleanup job: remove file failed, will retry next run",
			"table", table, "file", fileID, "error", err)
		return false
	}
	return true
}

// purgeLeftovers retries the physical removal of every deleted file row that
// still has a tus_upload_id. That covers removals that failed earlier (e.g. an
// SMB hiccup) and all stalled uploads deleted before removal included the flat
// TUS file — on SMB there is no orphan scan to catch those otherwise.
func (s *Scheduler) purgeLeftovers() {
	leftovers, err := s.stores.Files.ListUnpurged()
	if err != nil {
		slog.Error("cleanup job: list unpurged files", "error", err)
		return
	}
	var purged, waiting int
	var purgedBytes, waitingBytes int64
	for _, u := range leftovers {
		if s.removeAndPurge(u.Table, u.ID, u.StoragePath, u.TUSUploadID) {
			purged++
			purgedBytes += u.SizeBytes
		} else {
			waiting++
			waitingBytes += u.SizeBytes
		}
	}
	if purged > 0 {
		slog.Info("cleanup job: purged leftover files", "count", purged, "gb", float64(purgedBytes)/1_073_741_824)
	}
	if waiting > 0 {
		slog.Warn("cleanup job: deleted files still on storage, will retry next run",
			"count", waiting, "gb", float64(waitingBytes)/1_073_741_824)
	}
}

// ── Mail sending ─────────────────────────────────────────────────────────────

// ── Expiry summary builders ───────────────────────────────────────────────────

// expirySummary holds what the expiry summary mail shows.
type expirySummary struct {
	Settings   *store.Settings
	BaseURL    string
	Loc        *time.Location
	Transfer   store.Transfer
	History    []store.RecipientHistory
	Files      []store.File // complete files, in upload order
	GraceHours int
	DeletedAt  time.Time // set when an admin deleted the transfer; zero = it expired
}

func (e expirySummary) fileItems() []mail.FileItem {
	items := make([]mail.FileItem, 0, len(e.Files))
	for _, f := range e.Files {
		items = append(items, mail.FileItem{Name: f.OriginalName, Size: f.SizeBytes})
	}
	return items
}

// recipientSummary is what the summary says about one recipient: a status
// ("3 of 5 files"), one line per downloaded file with its download times,
// and the files not downloaded. HTML and text render the same summary.
type recipientSummary struct {
	Label   string
	Status  string
	Lines   []summaryLine
	Missing []string
}

type summaryLine struct {
	What  string
	Times string // "11 Sep 13:14, 12 Sep 09:02 (2×)"
}

// recipients builds the per-recipient summaries. A ZIP download records one
// event per file with the same time; when every file shows exactly the same
// times the lines collapse into one "All files" line instead of repeating the
// same time for each of 50 files.
func (e expirySummary) recipients() []recipientSummary {
	var out []recipientSummary
	for _, h := range e.History {
		byFile := make(map[string][]int64)
		byName := make(map[string][]int64) // events whose file row is gone
		var names []string
		known := make(map[string]bool, len(e.Files))
		for _, f := range e.Files {
			known[f.ID] = true
		}
		for _, ev := range h.Events {
			if ev.FileID.Valid && known[ev.FileID.String] {
				byFile[ev.FileID.String] = append(byFile[ev.FileID.String], ev.DownloadedAt)
				continue
			}
			if _, seen := byName[ev.OriginalName]; !seen {
				names = append(names, ev.OriginalName)
			}
			byName[ev.OriginalName] = append(byName[ev.OriginalName], ev.DownloadedAt)
		}

		rs := recipientSummary{Label: e.label(h)}
		downloaded := 0
		for _, f := range e.Files {
			if times, ok := byFile[f.ID]; ok {
				downloaded++
				rs.Lines = append(rs.Lines, summaryLine{What: f.OriginalName, Times: e.formatTimes(times)})
			} else {
				rs.Missing = append(rs.Missing, f.OriginalName)
			}
		}
		if downloaded == len(e.Files) && downloaded > 1 && sameTimes(rs.Lines) {
			rs.Lines = []summaryLine{{What: "All files", Times: rs.Lines[0].Times}}
		}
		for _, n := range names {
			rs.Lines = append(rs.Lines, summaryLine{What: n, Times: e.formatTimes(byName[n])})
		}

		total := len(e.Files)
		switch {
		case len(rs.Lines) == 0:
			rs.Status = "nothing downloaded"
			rs.Missing = nil // "nothing downloaded" already says it
		case total <= 1 || downloaded == 0:
			rs.Status = "downloaded"
		case downloaded == total:
			rs.Status = fmt.Sprintf("all %d files", total)
		default:
			rs.Status = fmt.Sprintf("%d of %d files", downloaded, total)
		}
		out = append(out, rs)
	}
	return out
}

// formatTimes lists download times per minute, oldest first; several
// downloads in the same minute show once with a count.
func (e expirySummary) formatTimes(unix []int64) string {
	var parts []string
	last, count := "", 0
	flush := func() {
		if count == 1 {
			parts = append(parts, last)
		} else if count > 1 {
			parts = append(parts, fmt.Sprintf("%s (%d×)", last, count))
		}
	}
	for _, u := range unix {
		t := time.Unix(u, 0).In(e.Loc).Format("2 Jan 15:04")
		if t == last {
			count++
			continue
		}
		flush()
		last, count = t, 1
	}
	flush()
	return strings.Join(parts, ", ")
}

func sameTimes(lines []summaryLine) bool {
	for _, l := range lines[1:] {
		if l.Times != lines[0].Times {
			return false
		}
	}
	return true
}

// label names a history row: the sender's own link and the one shared link
// of a link-only transfer carry the sender's address, which would read as
// if the sender downloaded their own files.
func (e expirySummary) label(h store.RecipientHistory) string {
	switch {
	case h.IsSender:
		return "You (your own link)"
	case !e.Transfer.NotifyRecipients:
		return "Your shared link"
	default:
		return h.Email
	}
}

func (e expirySummary) intro() string {
	title := "Your transfer"
	if e.Transfer.Title != "" {
		title = "Your transfer \"" + e.Transfer.Title + "\""
	}
	if !e.DeletedAt.IsZero() {
		return fmt.Sprintf("%s was deleted by an administrator on %s. The download links no longer work.",
			title, mail.FormatDate(e.DeletedAt, e.Loc))
	}
	return fmt.Sprintf("%s expired on %s. The download links no longer work.",
		title, mail.FormatDate(e.Transfer.ExpiresAt, e.Loc))
}

func (e expirySummary) cleanupNote() string {
	files := fmt.Sprintf("The files will be permanently deleted after a grace period of %d hours.", e.GraceHours)
	if !e.DeletedAt.IsZero() {
		files = "The files have been deleted."
	}
	return fmt.Sprintf("%s You are receiving this summary because expiry summaries are switched on for transfers you send with %s.",
		files, mail.CompanyName(e.Settings))
}

// buildExpirySummaryHTML renders the expiry summary mail as HTML. Per
// recipient: which files they downloaded and when, and which not
// (recipients()). Format (architecture.md §8):
//
//	bob@client.com: 3 of 5 files
//	  • a.mov: 11 Sep 13:14, 12 Sep 09:02
//	  • b.mov: 11 Sep 13:14
//	  Not downloaded: d.mov, e.mov
//
//	dave@client.com: nothing downloaded
func buildExpirySummaryHTML(e expirySummary) string {
	var rows strings.Builder
	for _, rs := range e.recipients() {
		bottom := "4px"
		if len(rs.Lines) == 0 && len(rs.Missing) == 0 {
			bottom = "16px"
		}
		fmt.Fprintf(&rows, `<p style="margin:0 0 %s;font-size:13px;color:#333;"><strong>%s</strong>: %s</p>`,
			bottom, html.EscapeString(rs.Label), html.EscapeString(rs.Status))
		if len(rs.Lines) == 0 && len(rs.Missing) == 0 {
			continue
		}
		rows.WriteString(`<ul style="margin:0 0 16px;padding-left:18px;font-size:13px;color:#555;line-height:1.6;">`)
		for _, l := range rs.Lines {
			fmt.Fprintf(&rows, `<li style="word-break:break-all;">%s: <span style="color:#888;">%s</span></li>`,
				html.EscapeString(l.What), html.EscapeString(l.Times))
		}
		if len(rs.Missing) > 0 {
			fmt.Fprintf(&rows, `<li style="color:#999;list-style:none;margin-left:-18px;">Not downloaded: %s</li>`,
				html.EscapeString(strings.Join(rs.Missing, ", ")))
		}
		rows.WriteString(`</ul>`)
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<p style="margin:0 0 16px;">Hello %s,</p>`, html.EscapeString(e.Transfer.SenderName))
	fmt.Fprintf(&b, `<p style="margin:0 0 20px;">%s Here is who downloaded what.</p>`, html.EscapeString(e.intro()))
	b.WriteString(`<p style="margin:0 0 10px;font-size:12px;font-weight:600;color:#888;text-transform:uppercase;letter-spacing:0.04em;">Downloads</p>`)
	b.WriteString(rows.String())
	if len(e.Files) > 0 {
		b.WriteString(`<p style="margin:20px 0 6px;font-size:12px;font-weight:600;color:#888;text-transform:uppercase;letter-spacing:0.04em;">Files</p>`)
		b.WriteString(mail.FileListHTML(e.fileItems()))
	}
	b.WriteString(mail.NoteHTML(e.cleanupNote()))

	return mail.Wrap(e.Settings, e.BaseURL, e.intro(), b.String())
}

func buildExpirySummaryText(e expirySummary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Hello %s,\n\n%s Here is who downloaded what.\n\n", e.Transfer.SenderName, e.intro())
	for _, rs := range e.recipients() {
		fmt.Fprintf(&b, "%s: %s\n", rs.Label, rs.Status)
		for _, l := range rs.Lines {
			fmt.Fprintf(&b, "  • %s: %s\n", l.What, l.Times)
		}
		if len(rs.Missing) > 0 {
			fmt.Fprintf(&b, "  Not downloaded: %s\n", strings.Join(rs.Missing, ", "))
		}
		b.WriteString("\n")
	}
	if len(e.Files) > 0 {
		b.WriteString("Files:\n" + mail.FileListText(e.fileItems()) + "\n")
	}
	fmt.Fprintf(&b, "--\n%s\n", e.cleanupNote())
	return b.String()
}
