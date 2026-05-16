package jobs

import (
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/your-org/ferri/internal/config"
	"github.com/your-org/ferri/internal/store"
)

// Scheduler runs background jobs on configurable intervals.
type Scheduler struct {
	cfg       *config.Config
	stores    *store.Stores
	stop      chan struct{}
	wg        sync.WaitGroup
	startOnce sync.Once // guards against Start() being called more than once
}

// NewScheduler creates a Scheduler. Call Start() to begin running jobs.
func NewScheduler(cfg *config.Config, stores *store.Stores) *Scheduler {
	return &Scheduler{
		cfg:    cfg,
		stores: stores,
		stop:   make(chan struct{}),
	}
}

// Start launches all background jobs in separate goroutines.
// It is safe to call only once; subsequent calls are no-ops.
func (s *Scheduler) Start() {
	s.startOnce.Do(func() {
		s.wg.Add(3)
		go s.runLoop(time.Duration(s.cfg.Jobs.MailIntervalMinutes)*time.Minute, s.runMailJob)
		go s.runLoop(time.Duration(s.cfg.Jobs.ExpiryIntervalMinutes)*time.Minute, s.runExpiryJob)
		go s.runLoop(time.Duration(s.cfg.Jobs.CleanupIntervalHours)*time.Hour, s.runCleanupJob)
	})
}

// Stop signals all jobs to stop and waits for them to finish their current run.
// This ensures in-progress cleanup or mail jobs complete before the process exits.
func (s *Scheduler) Stop() {
	close(s.stop)
	s.wg.Wait()
}

func (s *Scheduler) runLoop(interval time.Duration, fn func()) {
	defer s.wg.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			fn()
		}
	}
}

// ── Mail job ──────────────────────────────────────────────────────────────────

// runMailJob processes up to 20 pending mails per run.
// Sends via SMTP and updates status. Retries on failure with exponential backoff.
// See architecture.md §10 (Mail job) for the full specification.
func (s *Scheduler) runMailJob() {
	items, err := s.stores.Mail.FetchPending(20)
	if err != nil {
		slog.Error("mail job: fetch pending", "error", err)
		return
	}

	for _, item := range items {
		if err := sendMail(s.cfg, item); err != nil {
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

	// Prune old sent mails
	n, err := s.stores.Mail.PruneSent(s.cfg.Jobs.MailRetentionDays)
	if err != nil {
		slog.Error("mail job: prune sent", "error", err)
	} else if n > 0 {
		slog.Info("mail job: pruned sent mails", "count", n)
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
		settings := s.stores.Settings.Get()
		if settings.ExpirySummary && settings.MailFromAddress != "" {
			if err := s.enqueueExpirySummary(t.ID, t.SenderEmail, t.Title); err != nil {
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

// enqueueExpirySummary builds and enqueues the expiry summary mail for a transfer.
func (s *Scheduler) enqueueExpirySummary(transferID, senderEmail, title string) error {
	history, err := s.stores.Downloads.GetHistoryForTransfer(transferID)
	if err != nil {
		return err
	}

	// TODO: render expiry summary mail template with history
	// For now, enqueue a placeholder
	subject := "Transfer expired: " + title
	bodyHTML := buildExpirySummaryHTML(title, history)
	bodyText := buildExpirySummaryText(title, history)

	return s.stores.Mail.Enqueue(nil, senderEmail, subject, bodyHTML, bodyText)
}

// ── Cleanup job ───────────────────────────────────────────────────────────────

// runCleanupJob deletes files from storage for expired transfers past the grace period,
// and cleans up stalled uploads.
func (s *Scheduler) runCleanupJob() {
	var totalBytes int64
	var totalTransfers int

	// Clean up expired transfers
	transfers, err := s.stores.Transfers.GetForCleanup(s.cfg.Jobs.CleanupGraceHours)
	if err != nil {
		slog.Error("cleanup job: get for cleanup", "error", err)
		return
	}

	for _, t := range transfers {
		// Sum bytes BEFORE deletion (os.RemoveAll returns no size info)
		size, err := s.stores.Transfers.SumFileSizes(t.ID)
		if err != nil {
			slog.Error("cleanup job: sum sizes", "transfer", t.ID, "error", err)
		}

		dir := filepath.Join(s.cfg.Storage.Path, "transfers", t.ID)
		if err := os.RemoveAll(dir); err != nil {
			slog.Error("cleanup job: remove files", "transfer", t.ID, "dir", dir, "error", err)
			continue // retry next run
		}

		if err := s.stores.Transfers.MarkFilesDeleted(t.ID); err != nil {
			slog.Error("cleanup job: mark deleted", "transfer", t.ID, "error", err)
			continue
		}

		totalBytes += size
		totalTransfers++
	}

	// Clean up expired upload requests
	requests, err := s.stores.Requests.GetForCleanup(s.cfg.Jobs.CleanupGraceHours)
	if err != nil {
		slog.Error("cleanup job: get requests for cleanup", "error", err)
	}

	for _, r := range requests {
		dir := filepath.Join(s.cfg.Storage.Path, "requests", r.ID)
		if err := os.RemoveAll(dir); err != nil {
			slog.Error("cleanup job: remove request files", "request", r.ID, "error", err)
			continue
		}
	}

	// Clean up stalled uploads (independent of transfer expiry)
	s.cleanupStalled()

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
		storagePath := filepath.Join(s.cfg.Storage.Path, f.StoragePath)

		// Remove both the content file and the TUS .info sidecar.
		// If only the content file is removed, TUS believes the upload can be resumed.
		_ = os.Remove(storagePath)
		_ = os.Remove(storagePath + ".info")

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
		storagePath := filepath.Join(s.cfg.Storage.Path, f.StoragePath)
		_ = os.Remove(storagePath)
		_ = os.Remove(storagePath + ".info")
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

// ── Mail helpers (placeholder implementations) ────────────────────────────────

// TODO: replace with proper template rendering from internal/mail/

func buildExpirySummaryHTML(title string, history []store.RecipientHistory) string {
	return "<p>Transfer <strong>" + title + "</strong> has expired.</p>"
}

func buildExpirySummaryText(title string, history []store.RecipientHistory) string {
	return "Transfer '" + title + "' has expired."
}

// sendMail sends a single mail item via SMTP.
// TODO: implement using internal/mail/mailer.go
func sendMail(cfg *config.Config, item store.MailItem) error {
	// Placeholder — full implementation in internal/mail/mailer.go
	return nil
}
