package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/Atash03/BugHan/internal/db"
	"github.com/Atash03/BugHan/internal/testdb"
)

// DropPartitions must parse the bounds Postgres normalizes into
// pg_get_expr (space-separated, server timezone), not just the RFC3339 form
// we create them with — otherwise old partitions silently survive.
func TestDropPartitionsHandlesNormalizedBounds(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	oldMonth := time.Now().UTC().AddDate(0, -4, 0)
	if err := db.EnsurePartitions(ctx, pool, []time.Time{oldMonth, time.Now().UTC()}); err != nil {
		t.Fatalf("EnsurePartitions: %v", err)
	}

	oldName := "events_part_" + oldMonth.Format("2006_01")
	if err := db.DropPartitions(ctx, pool, "events_part", time.Now().UTC()); err != nil {
		t.Fatalf("DropPartitions: %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_class WHERE relname = $1`, oldName).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("old partition %s survived DropPartitions", oldName)
	}
}
