package main

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/caarlos0/env/v11"
	feed "github.com/kontrolplane/feed"
	"github.com/kontrolplane/feed/internal/configuration"
	"github.com/kontrolplane/feed/internal/database"
	"github.com/kontrolplane/feed/internal/handler"
	"github.com/kontrolplane/feed/internal/store"
	"github.com/kontrolplane/feed/internal/worker"
)

// version is stamped at build time with -ldflags "-X main.version=...".
// The Dockerfile and Makefile both pass it; without this variable existing
// that flag is silently a no-op, which is how it shipped before.
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Set up structured logging
	logLevel := slog.LevelInfo
	logHandler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		AddSource: false,
		Level:     logLevel,
	})
	logger := slog.New(logHandler)
	// Packages that log without a handle of their own — the CSRF middleware's
	// rejection warnings, the OPML importer's per-entry skips — call
	// slog.Default(). Without this they bypass the JSON handler configured
	// here and land on stderr as unstructured text.
	slog.SetDefault(logger)

	// Parse environment variables
	var cfg configuration.FeedServiceConfiguration
	if err := env.Parse(&cfg); err != nil {
		logger.Error("unable to parse environment variables", slog.Any("error", err))
		os.Exit(1)
	}

	if cfg.Debug {
		logHandler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			AddSource: true,
			Level:     slog.LevelDebug,
		})
		logger = slog.New(logHandler)
		slog.SetDefault(logger)
	}

	fmt.Fprintf(os.Stderr, `
  ▐▐▐  kontrolplane/feed

  version:     %s
  listening:   http://localhost:%d
  driver:      %s
  refresh:     %s

`, version, cfg.Port, cfg.DatabaseDriver, cfg.RefreshInterval)

	// Create database pool
	pool, err := database.CreatePool(ctx, cfg, logger)
	if err != nil {
		logger.Error("failed to create database pool", slog.Any("error", err))
		os.Exit(1)
	}
	defer pool.Close()

	// Run migrations
	if err := database.Migrate(ctx, cfg, logger); err != nil {
		logger.Error("failed to run database migration", slog.Any("error", err))
		os.Exit(1)
	}

	// Create store
	s := store.New(pool, cfg.DatabaseDriver)

	// Import feeds from file if configured
	if cfg.FeedsFile != "" {
		n, err := store.ImportOPMLFile(ctx, s, cfg.FeedsFile)
		if err != nil {
			logger.Error("failed to import feeds file", slog.String("path", cfg.FeedsFile), slog.Any("error", err))
			os.Exit(1)
		}
		logger.Info("imported feeds from file", slog.String("path", cfg.FeedsFile), slog.Int("feeds", n))
	}

	// Seed database with default feeds when explicitly enabled
	if cfg.Seed {
		if err := store.Seed(ctx, s); err != nil {
			logger.Error("failed to seed database", slog.Any("error", err))
			os.Exit(1)
		}
	}

	// Create handler. It takes the pool as well as the store so /healthz can
	// Ping the database directly instead of running a full page render.
	h := handler.New(s, pool, logger, cfg)

	// Start background feed fetcher. Start returns a wait function: the
	// fetcher used to be fire-and-forget, so main could return — and
	// `defer pool.Close()` could run — while fetchAll was mid-write.
	fetcher := worker.NewFetcher(s, logger, cfg, func(t time.Time) {
		h.SetLastSync(t)
	})
	waitForFetcher := fetcher.Start(ctx)

	// POST /items/{id}/refetch re-pulls a single article on demand. Wired
	// before the server goroutine starts, so the field is never written
	// concurrently with a request reading it.
	h.SetFetcher(fetcher)

	// Create router
	mux := http.NewServeMux()
	h.Register(mux)

	// Static files, served from the embedded FS rather than from disk — see
	// static.go. fs.Sub strips the leading "static/" so the embedded paths
	// line up with the /static/ URL prefix.
	staticRoot, err := fs.Sub(feed.StaticFS, "static")
	if err != nil {
		logger.Error("failed to open embedded static assets", slog.Any("error", err))
		os.Exit(1)
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(staticRoot)))

	// Create http server
	httpServer := &http.Server{
		Addr: fmt.Sprintf(":%d", cfg.Port),
		// Secure = SecurityHeaders(CSRFGuard(next)): the headers wrap the guard
		// so that the guard's own 403 still carries the CSP. LogRequests stays
		// outermost so it observes the final status of every response.
		Handler:      handler.LogRequests(logger)(handler.Secure(mux)),
		IdleTimeout:  60 * time.Second,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	// Start server in goroutine
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("failed to start the server", slog.Any("error", err))
			os.Exit(1)
		}
	}()

	// Wait for interrupt signal
	<-ctx.Done()

	// Graceful shutdown
	logger.Info("shutting down gracefully...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("failed to shut down gracefully", slog.Any("error", err))
		os.Exit(1)
	}

	// Only now is it safe to let the deferred pool.Close() run: the fetcher
	// writes to the database on its own goroutine, and ctx is already
	// cancelled, so this returns as soon as the in-flight cycle unwinds.
	waitForFetcher()

	logger.Info("service shut down successfully")
}
