// Command bughan is BugHan's single binary: HTTP server (UI + API + ingest)
// and administrative subcommands.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Atash03/BugHan/internal/auth"
	"github.com/Atash03/BugHan/internal/config"
	"github.com/Atash03/BugHan/internal/db"
	"github.com/Atash03/BugHan/internal/httpapi"
	"github.com/Atash03/BugHan/internal/mail"
	"github.com/Atash03/BugHan/internal/worker"
	"golang.org/x/term"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve":
			runServe(log)
		case "migrate":
			runMigrate(log)
		case "reset-password":
			runResetPassword(log)
		case "create-org":
			runCreateOrg(log)
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
  bughan [serve]        Run the server (UI + API + ingest [+ embedded worker])
  bughan migrate        Apply pending database migrations and exit
  bughan reset-password <email>          Set a new password (prompts or BUGHAN_NEW_PASSWORD)
  bughan create-org <name> <owner-email> Create an organization owned by an existing user
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

	if cfg.WorkerEmbedded {
		w := worker.New(pool, log)
		if err := w.RegisterRetention(worker.RetentionConfig{
			EventDays:         cfg.RetentionEventDays,
			TransactionDays:   cfg.RetentionTransactionDays,
			SessionDays:       cfg.RetentionSessionDays,
			SessionRollupDays: cfg.RetentionSessionDays * 3, // rollups outlive raw sessions
			FeedbackDays:      cfg.RetentionFeedbackDays,
			ReleaseFileDays:   cfg.RetentionReleaseDays,
		}); err != nil {
			log.Error("schedule retention sweep", "err", err)
			os.Exit(1)
		}
		if err := w.RegisterRollupMaintenance(); err != nil {
			log.Error("schedule rollup maintenance", "err", err)
			os.Exit(1)
		}
		w.RegisterAlerts(worker.AlertsDeps{
			Cfg: cfg, Mailer: mail.New(cfg, log), Log: log,
		})
		go w.Start(ctx)
		log.Info("embedded worker started")
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	log.Info("bughan listening", "addr", cfg.BindAddr, "public_url", cfg.PublicURL)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server", "err", err)
		os.Exit(1)
	}
	log.Info("bughan stopped")
}

func runResetPassword(log *slog.Logger) {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: bughan reset-password <email>")
		os.Exit(2)
	}
	email := strings.ToLower(strings.TrimSpace(os.Args[2]))
	cfg := mustConfig(log)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("open database", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	newPassword := os.Getenv("BUGHAN_NEW_PASSWORD")
	if newPassword == "" {
		fmt.Print("New password (min 8 chars): ")
		pw, _ := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		newPassword = string(pw)
	}
	if len(newPassword) < 8 {
		fmt.Fprintln(os.Stderr, "password too short")
		os.Exit(1)
	}
	hash, err := auth.HashPassword(newPassword)
	if err != nil {
		log.Error("hash", "err", err)
		os.Exit(1)
	}
	ct, err := pool.Exec(ctx, `UPDATE users SET password_hash = $2, reset_token = NULL WHERE email = $1`, email, hash)
	if err != nil || ct.RowsAffected() == 0 {
		log.Error("no such user", "email", email)
		os.Exit(1)
	}
	log.Info("password updated", "email", email)
}

func runCreateOrg(log *slog.Logger) {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: bughan create-org <name> <owner-email>")
		os.Exit(2)
	}
	name := os.Args[2]
	email := strings.ToLower(strings.TrimSpace(os.Args[3]))
	cfg := mustConfig(log)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("open database", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	var userID string
	if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE email = $1`, email).Scan(&userID); err != nil {
		log.Error("no such user; sign up first", "email", email)
		os.Exit(1)
	}
	var orgID string
	slug := auth.Slugify(name)
	if err := pool.QueryRow(ctx, `
		INSERT INTO organizations (id, name, slug)
		VALUES (gen_random_uuid(), $1, $2)
		ON CONFLICT (slug) DO UPDATE SET name = EXCLUDED.name
		RETURNING id, slug`, name, slug).Scan(&orgID, &slug); err != nil {
		log.Error("create org", "err", err)
		os.Exit(1)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO memberships (id, org_id, user_id, role)
		 VALUES (gen_random_uuid(), $1, $2, 'owner')
		 ON CONFLICT (org_id, user_id) DO UPDATE SET role = 'owner'`, orgID, userID); err != nil {
		log.Error("grant ownership", "err", err)
		os.Exit(1)
	}
	log.Info("organization created", "slug", slug, "owner", email)
}
