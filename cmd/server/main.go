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

	"github.com/BartVermeir/Ferri/internal/config"
	"github.com/BartVermeir/Ferri/internal/db"
	"github.com/BartVermeir/Ferri/internal/handler"
	"github.com/BartVermeir/Ferri/internal/jobs"
	"github.com/BartVermeir/Ferri/internal/storage"
	"github.com/BartVermeir/Ferri/internal/store"
	ferritls "github.com/BartVermeir/Ferri/internal/tus"
)

// Version is set at build time via -ldflags "-X main.Version=...".
// Defaults to "dev" for local `go run`/`go build` without that flag.
var Version = "dev"

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

	// ── Templates ──────────────────────────────────────────────────────────
	// Bind the template date formatter to the configured timezone. Without
	// this call formatDate stays on UTC regardless of server.timezone.
	handler.InitTemplates(cfg.Server.Location)
	handler.SetVersion(Version)

	for _, k := range cfg.UnknownKeys {
		slog.Warn("config.yaml: unknown key, ignored — check for a typo", "key", k)
	}
	if overlap := cfg.ProxiesInAllowlist(); len(overlap) > 0 {
		slog.Warn("trusted_proxies overlap ip_allowlist: a request forwarded without X-Real-IP would be treated as internal — remove the proxy IP from ip_allowlist",
			"proxies", overlap)
	}

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

	// ── Storage Manager ────────────────────────────────────────────────────
	// Initialize from current settings. Defaults to local backend.
	// The Manager can be hot-swapped at runtime via the admin settings UI.
	initialSettings := stores.Settings.Get()
	initialBackend, err := storage.FromSettings(initialSettings, cfg, cfg.Admin.Token)
	if err != nil {
		slog.Warn("storage: could not initialize from settings, falling back to local", "error", err)
		initialBackend = storage.NewLocalBackend(cfg.Storage.Path)
	}
	storageMgr := storage.NewManager(initialBackend)
	defer storageMgr.Close()

	// ── TUS handler ────────────────────────────────────────────────────────
	tusHandler, err := ferritls.NewHandler(cfg, stores, storageMgr)
	if err != nil {
		slog.Error("failed to create TUS handler", "error", err)
		os.Exit(1)
	}

	// ── Jobs ───────────────────────────────────────────────────────────────
	scheduler := jobs.NewScheduler(cfg, stores, storageMgr)
	scheduler.Start()
	defer scheduler.Stop()

	// ── Router ─────────────────────────────────────────────────────────────
	r := newRouter(cfg, database, stores, storageMgr, tusHandler, scheduler)

	// ── Server ─────────────────────────────────────────────────────────────
	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      r,
		ReadTimeout:  0, // TUS uploads can take hours
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
		// Only the headers: a client that trickles them in holds a
		// connection forever otherwise (slowloris, audit L4). Bodies are
		// not limited, so long TUS PATCHes are unaffected.
		ReadHeaderTimeout: 30 * time.Second,
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
