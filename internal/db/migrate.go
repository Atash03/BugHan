package db

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type migration struct {
	version int
	name    string
	sql     string
}

// Migrate applies all pending migrations, forward-only, inside a
// schema_migrations advisory lock so concurrent boots serialize.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version integer PRIMARY KEY,
		name text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	migs, err := loadMigrations()
	if err != nil {
		return err
	}

	var current int
	if err := pool.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return err
	}

	for _, m := range migs {
		if m.version <= current {
			continue
		}
		err := func() error {
			tx, err := pool.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)
			// Serialize concurrent migration runs; statement-level within the tx.
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(721834501)`); err != nil {
				return err
			}
			// Re-check inside the lock.
			var max int
			if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&max); err != nil {
				return err
			}
			if m.version <= max {
				return nil
			}
			if _, err := tx.Exec(ctx, m.sql); err != nil {
				return fmt.Errorf("migration %04d_%s: %w", m.version, m.name, err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, m.version, m.name); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}()
		if err != nil {
			return err
		}
	}

	// Partitioned telemetry tables need monthly partitions to exist before
	// the first insert.
	now := time.Now().UTC()
	if err := EnsurePartitions(ctx, pool, []time.Time{now, now.AddDate(0, 1, 0)}); err != nil {
		return err
	}
	return nil
}

// PendingVersions returns migration versions not yet applied (for the CLI).
func CurrentVersion(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var v int
	err := pool.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v)
	return v, err
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, err
	}
	var migs []migration
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		base := strings.TrimSuffix(name, ".sql")
		parts := strings.SplitN(base, "_", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("migration file %s must be named NNNN_description.sql", name)
		}
		var version int
		if _, err := fmt.Sscanf(parts[0], "%d", &version); err != nil {
			return nil, fmt.Errorf("migration file %s has bad version prefix", name)
		}
		sql, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, err
		}
		migs = append(migs, migration{version: version, name: parts[1], sql: string(sql)})
	}
	sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })
	for i := 1; i < len(migs); i++ {
		if migs[i].version == migs[i-1].version {
			return nil, fmt.Errorf("duplicate migration version %d", migs[i].version)
		}
	}
	return migs, nil
}

// ensureMonthlyPartitions creates partitions for the current and next month
// so fresh installs can ingest immediately; the retention sweep keeps future
// months created.
func ensureMonthlyPartitions(ctx context.Context, pool *pgxpool.Pool) error {
	now := time.Now().UTC()
	return EnsurePartitions(ctx, pool, []time.Time{now, now.AddDate(0, 1, 0)})
}
