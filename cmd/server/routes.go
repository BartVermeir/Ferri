package main

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"

	"github.com/BartVermeir/Ferri/internal/config"
	"github.com/BartVermeir/Ferri/internal/handler"
	"github.com/BartVermeir/Ferri/internal/jobs"
	"github.com/BartVermeir/Ferri/internal/middleware"
	"github.com/BartVermeir/Ferri/internal/storage"
	"github.com/BartVermeir/Ferri/internal/store"
)

// newRouter wires every route with its middleware. It lives outside main so
// router_test.go can test the real wiring: a route that exists only in a
// handler test's own router is how H3 (a missing POST route) slipped through.
func newRouter(cfg *config.Config, database *sql.DB, stores *store.Stores, storageMgr *storage.Manager, tusHandler http.Handler, scheduler *jobs.Scheduler) http.Handler {
	r := chi.NewRouter()

	// Global middleware
	r.Use(chimiddleware.RequestID)
	r.Use(middleware.Recovery())
	r.Use(middleware.SecurityHeaders(cfg.Server.SecureCookies))
	r.Use(middleware.InjectSettings(stores.Settings))

	// Brute-force limiter for credential submissions: 10 attempts per minute,
	// keyed per (client IP, path) — so per-token for password pages, per-IP for login.
	authLimiter := middleware.NewRateLimiter(10, time.Minute, cfg.TrustedProxies)

	// Public routes
	r.Get("/health", handler.Health(database))
	r.Handle("/static/*", http.StripPrefix("/static/", handler.Static()))
	r.Get("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/static/favicon.svg", http.StatusMovedPermanently)
	})
	// Serve the uploaded logo from local disk (no directory listing).
	r.Handle("/static/logo/*", http.StripPrefix("/static/logo/", handler.LogoFileServer(cfg.Storage.Path+"/logo")))
	r.Get("/dl/{token}", handler.DownloadPage(cfg, stores))
	r.With(authLimiter.Middleware).Post("/dl/{token}", handler.DownloadPassword(cfg, stores))
	r.Get("/dl/{token}/file/{fileID}", handler.DownloadFile(cfg, stores, storageMgr))
	r.Get("/dl/{token}/zip", handler.DownloadZIP(cfg, stores, storageMgr))
	r.Get("/ul/{token}", handler.UploadPage(cfg, stores))
	r.With(authLimiter.Middleware).Post("/ul/{token}", handler.UploadPassword(cfg, stores))
	r.Post("/ul/{token}/complete", handler.UploadComplete(cfg, stores))
	r.Get("/ul/{token}/files", handler.RequestDownloadPage(cfg, stores))
	r.With(authLimiter.Middleware).Post("/ul/{token}/files", handler.RequestFilesPassword(cfg, stores))
	r.Get("/ul/{token}/file/{fileID}", handler.RequestDownloadFile(cfg, stores, storageMgr))
	r.Get("/ul/{token}/zip", handler.RequestDownloadZIP(cfg, stores, storageMgr))
	// TUS: use http.StripPrefix so tusd sees the path without /tus prefix
	r.Mount("/tus", http.StripPrefix("/tus", tusHandler))

	// IP-restricted routes (internal network only)
	forbidden := handler.Forbidden()
	r.Group(func(r chi.Router) {
		r.Use(middleware.IPAllow(cfg.IPAllowlist, cfg.TrustedProxies, forbidden))
		r.Use(middleware.CSRFProtect(cfg.Server.BaseURL))
		r.Get("/", handler.SendPage(cfg, stores))
		r.Post("/send", handler.SendCreate(cfg, stores))
		r.Get("/request", handler.RequestPage(cfg, stores))
		r.Post("/request", handler.RequestCreate(cfg, stores))
		// Manage links (DEC-043): the token alone opens them, so internal only.
		r.Get("/manage/{token}", handler.ManagePage(cfg, stores))
		r.Post("/manage/{token}/extend", handler.ManageExtend(cfg, stores))
		r.Post("/manage/{token}/delete", handler.ManageDelete(cfg, stores, storageMgr, scheduler))
	})

	// Admin routes (IP-restricted + session cookie)
	r.Group(func(r chi.Router) {
		r.Use(middleware.IPAllow(cfg.IPAllowlist, cfg.TrustedProxies, forbidden))
		r.Use(middleware.CSRFProtect(cfg.Server.BaseURL))
		r.Get("/admin/login", handler.AdminLogin(cfg))
		r.With(authLimiter.Middleware).Post("/admin/login", handler.AdminLoginPost(cfg))
		r.Group(func(r chi.Router) {
			r.Use(middleware.AdminAuth(cfg))
			r.Get("/admin", handler.AdminDashboard(cfg, stores))
			r.Post("/admin/cleanup", handler.AdminForceCleanup(cfg, scheduler))
			r.Post("/admin/orphans/clean", handler.AdminOrphanClean(cfg, stores))
			r.Get("/admin/transfers", handler.AdminTransfers(cfg, stores))
			r.Get("/admin/transfers/{id}/files", handler.AdminTransferFiles(cfg, stores))
			r.Get("/admin/transfers/{id}/file/{fileID}", handler.AdminTransferFile(cfg, stores, storageMgr))
			r.Post("/admin/transfers/{id}/delete", handler.AdminTransferDelete(cfg, stores, storageMgr, scheduler))
			r.Post("/admin/requests/{id}/delete", handler.AdminRequestDelete(cfg, stores, storageMgr))
			r.Get("/admin/mail", handler.AdminMail(cfg, stores))
			r.Post("/admin/mail/{id}/retry", handler.AdminMailRetry(cfg, stores))
			r.Post("/admin/mail/{id}/delete", handler.AdminMailDelete(cfg, stores))
			r.Get("/admin/settings", handler.AdminSettings(cfg, stores))
			r.Post("/admin/settings", handler.AdminSettingsSave(cfg, stores))
			r.Post("/admin/settings/logo", handler.AdminLogoUpload(cfg, stores))
			r.Post("/admin/settings/logo/delete", handler.AdminLogoDelete(cfg, stores))
			r.Post("/admin/settings/storage", handler.AdminStorageSave(cfg, stores, storageMgr))
			r.Post("/admin/settings/storage/test", handler.AdminStorageTest(cfg, stores))
			r.Post("/admin/logout", handler.AdminLogout(cfg))
		})
	})

	return r
}
