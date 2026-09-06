// Command bughan is BugHan's single binary: HTTP server (UI + API + ingest)
// and administrative subcommands.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Atash03/BugHan/internal/config"
	"github.com/Atash03/BugHan/internal/db"
	"github.com/Atash03/BugHan/internal/httpapi"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve":
			runServe(log)
		case "migrate":
			runMigrate(log)
		case "-h", "--help", "help":
			usage()
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
			usage()
			os.Exit(2)
		}
		return
	}
	runServe(log)
}

func usage() {
	fmt.Print(`bughan — self-hosted, Sentry-compatible error and performance tracking

Usage:
  bughan [serve]     Run the server (UI + API + ingest [+ embedded worker])
  bughan migrate     Apply pending database migrations and exit
`)
}

func mustConfig(log *slog.Logger) *config.Config {
	cfg, err := config.Load()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	return cfg
}

func runMigrate(log *slog.Logger) {
	cfg := mustConfig(log)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("open database", "err", err)
		os.Exit(1)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		log.Error("migrate", "err", err)
		os.Exit(1)
	}
	v, _ := db.CurrentVersion(ctx, pool)
	log.Info("migrations applied", "version", v)
}

func runServe(log *slog.Logger) {
	cfg := mustConfig(log)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("open database", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := db.Migrate(ctx, pool); err != nil {
		log.Error("migrate", "err", err)
		os.Exit(1)
	}

	srv := httpapi.New(cfg, pool, log)
	httpSrv := &http.Server{Addr: cfg.BindAddr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	log.Info("bughan listening", "addr", cfg.BindAddr, "public_url", cfg.PublicURL)
	if err := httpSrv.ListenAndServe(); err != nil && err != context.Canceled {
		log.Error("server", "err", err)
		os.Exit(1)
	}
	log.Info("bughan stopped")
}
