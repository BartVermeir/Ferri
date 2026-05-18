package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"

	"github.com/your-org/ferri/internal/config"
	"github.com/your-org/ferri/internal/db"
	"github.com/your-org/ferri/internal/handler"
	"github.com/your-org/ferri/internal/jobs"
	"github.com/your-org/ferri/internal/middleware"
	"github.com/your-org/ferri/internal/store"
	ferritls "github.com/your-org/ferri/internal/tus"
)

func main() {
	// ── Flags ──────────────────────────────────────────────────────────────
	healthCheck := flag.Bool("health", false, "perform health check and exit")
	healthPort := flag.Int("port", 8080, "port to use for health check (matches server.port in config)")
	flag.Parse()

	if *healthCheck {
		url := fmt.Sprintf("http://localhost:%d/health", *healthPort)
		resp, err := http.Get(url)
		if err != nil || resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		os.Exit(0)
	}

	// ── Config ─────────────────────────────────────────────────────────────
	cfg, err := config.Load(os.Getenv("CONFIG_PATH"))
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// ── Logger ─────────────────────────────────────────────────────────────
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	// ── Database ───────────────────────────────────────────────────────────
	database, err := db.Open(cfg.DB.Path)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		os.Exit(1)
	}
	defer database.Close()

	if err := db.Migrate(database); err != nil {
		slog.Error("migration failed", "error", err)
		os.Exit(1)
	}

	if err := db.StartupHooks(database); err != nil {
		slog.Error("startup hooks failed", "error", err)
		os.Exit(1)
	}

	// ── Stores ─────────────────────────────────────────────────────────────
	stores := store.New(database)

	// ── TUS handler ────────────────────────────────────────────────────────
	tusHandler, err := ferritls.NewHandler(cfg, stores)
	if err != nil {
		slog.Error("failed to create TUS handler", "error", err)
		os.Exit(1)
	}

	// ── Router ─────────────────────────────────────────────────────────────
	r := chi.NewRouter()

	// Global middleware
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.RequestID)
	r.Use(middleware.Recovery())
	r.Use(middleware.InjectSettings(stores.Settings))

	// Public routes
	r.Get("/health", handler.Health())
	r.Handle("/static/*", http.StripPrefix("/static/", handler.Static()))
	r.Get("/dl/{token}", handler.DownloadPage(cfg, stores))
	r.Post("/dl/{token}", handler.DownloadPassword(cfg, stores))
	r.Get("/dl/{token}/file/{fileID}", handler.DownloadFile(cfg, stores))
	r.Get("/ul/{token}", handler.UploadPage(cfg, stores))
	r.Post("/ul/{token}", handler.UploadPassword(cfg, stores))
	r.Post("/ul/{token}/complete", handler.UploadComplete(cfg, stores))
	// TUS requires all HTTP methods — Mount with wildcard to pass everything through
	r.Handle("/tus/*", tusHandler)
	r.Handle("/tus/", tusHandler)

	// IP-restricted routes (internal network only)
	r.Group(func(r chi.Router) {
		r.Use(middleware.IPAllow(cfg.IPAllowlist))
		r.Get("/", handler.SendPage(cfg, stores))
		r.Post("/send", handler.SendCreate(cfg, stores))
		r.Get("/request", handler.RequestPage(cfg, stores))
		r.Post("/request", handler.RequestCreate(cfg, stores))
	})

	// Admin routes (IP-restricted + session cookie)
	r.Group(func(r chi.Router) {
		r.Use(middleware.IPAllow(cfg.IPAllowlist))
		r.Get("/admin/login", handler.AdminLogin(cfg))
		r.Post("/admin/login", handler.AdminLoginPost(cfg))
		r.Group(func(r chi.Router) {
			r.Use(middleware.AdminAuth(cfg))
			r.Get("/admin", handler.AdminDashboard(cfg, stores))
			r.Get("/admin/transfers", handler.AdminTransfers(cfg, stores))
			r.Post("/admin/transfers/{id}/delete", handler.AdminTransferDelete(cfg, stores))
			r.Get("/admin/mail", handler.AdminMail(cfg, stores))
			r.Post("/admin/mail/{id}/retry", handler.AdminMailRetry(cfg, stores))
			r.Post("/admin/mail/{id}/delete", handler.AdminMailDelete(cfg, stores))
			r.Get("/admin/settings", handler.AdminSettings(cfg, stores))
			r.Post("/admin/settings", handler.AdminSettingsSave(cfg, stores))
			r.Post("/admin/logout", handler.AdminLogout())
		})
	})

	// ── Jobs ───────────────────────────────────────────────────────────────
	scheduler := jobs.NewScheduler(cfg, stores)
	scheduler.Start()
	defer scheduler.Stop()

	// ── Server ─────────────────────────────────────────────────────────────
	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      r,
		ReadTimeout:  0, // TUS uploads can take hours
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		slog.Info("server listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-stop
	slog.Info("shutting down...")

	shutdownTimeout := time.Duration(cfg.Server.ShutdownTimeoutSeconds) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("shutdown error", "error", err)
	}

	slog.Info("stopped")
}
