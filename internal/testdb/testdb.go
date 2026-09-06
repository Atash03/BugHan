// Package testdb provides a throwaway Postgres database for integration
// tests. Tests skip (not fail) when no local Postgres is reachable, so the
// suite stays green on machines without Docker; CI always provides Postgres.
package testdb

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Atash03/BugHan/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

// New creates a uniquely-named database with all migrations applied and
// returns its pool plus the full URL. Registers cleanup on t.
func New(t *testing.T) *pgxpool.Pool {
	t.Helper()

	baseURL := os.Getenv("BUGHAN_TEST_DATABASE_URL")
	if baseURL == "" {
		baseURL = "postgres://bughan:bughan@localhost:5432/postgres?sslmode=disable"
	}
	adminURL := baseURL
	if dbURL := os.Getenv("DATABASE_URL"); dbURL != "" {
		// CI convenience: derive an admin URL from the configured test DB.
		adminURL = dbURL
	}

	ctx := context.Background()
	adminPool, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		t.Skipf("no local postgres for integration tests: %v", err)
	}
	defer adminPool.Close()
	if err := adminPool.Ping(ctx); err != nil {
		t.Skipf("no local postgres for integration tests: %v", err)
	}

	name := fmt.Sprintf("bughan_test_%d", time.Now().UnixNano())
	if _, err := adminPool.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %q TEMPLATE template0`, name)); err != nil {
		t.Skipf("cannot create test database: %v", err)
	}

	testURL := replaceDBName(adminURL, name)
	pool, err := pgxpool.New(ctx, testURL)
	if err != nil {
		t.Fatalf("connect test db: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		pool.Close()
		dropDB(adminURL, name)
		t.Fatalf("migrate test db: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropDB(adminURL, name)
	})
	return pool
}

func dropDB(adminURL, name string) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		return
	}
	defer pool.Close()
	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, name))
}

// replaceDBName swaps the database component of a connection string URL.
func replaceDBName(url, name string) string {
	if i := indexOfDBName(url); i >= 0 {
		end := len(url)
		if q := indexByteFrom(url, i, '?'); q >= 0 {
			end = q
		}
		return url[:i] + name + url[end:]
	}
	return url
}

func indexOfDBName(url string) int {
	// after scheme://authority/
	slashes := 0
	for i := 0; i < len(url); i++ {
		if url[i] == '/' {
			slashes++
			if slashes == 3 {
				return i + 1
			}
		}
	}
	return -1
}

func indexByteFrom(s string, from int, b byte) int {
	for i := from; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
