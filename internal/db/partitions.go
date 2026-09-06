package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// partitionedTables maps parent table → timestamp column used for the range.
var partitionedTables = []struct{ table, column string }{
	{"events_part", "timestamp"},
	{"transactions_part", "timestamp"},
	{"sessions_part", "started"},
}

// EnsurePartitions creates monthly partitions for the given months if missing.
// Called at boot (current + next month) and daily by the retention sweep.
func EnsurePartitions(ctx context.Context, pool *pgxpool.Pool, months []time.Time) error {
	for _, m := range months {
		start := time.Date(m.Year(), m.Month(), 1, 0, 0, 0, 0, time.UTC)
		end := start.AddDate(0, 1, 0)
		name := start.Format("2006_01")
		for _, t := range partitionedTables {
			q := fmt.Sprintf(
				`CREATE TABLE IF NOT EXISTS %s_%s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')`,
				t.table, name, t.table, start.Format(time.RFC3339), end.Format(time.RFC3339))
			if _, err := pool.Exec(ctx, q); err != nil {
				return fmt.Errorf("create partition %s_%s: %w", t.table, name, err)
			}
		}
	}
	return nil
}

// DropPartitions removes monthly partitions of a parent table whose upper
// bound is strictly older than cutoff.
func DropPartitions(ctx context.Context, pool *pgxpool.Pool, table string, cutoff time.Time) error {
	rows, err := pool.Query(ctx, `
		SELECT c.relname, pg_get_expr(c.relpartbound, c.oid)
		FROM pg_class c JOIN pg_inherits i ON i.inhrelid = c.oid
		JOIN pg_class p ON p.oid = i.inhparent
		WHERE p.relname = $1`, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	type part struct{ name, bound string }
	var parts []part
	for rows.Next() {
		var p part
		if err := rows.Scan(&p.name, &p.bound); err != nil {
			return err
		}
		parts = append(parts, p)
	}
	for _, p := range parts {
		// Bound format: FOR VALUES FROM ('2026-01-01 …') TO ('2026-02-01 …')
		_, rest, _ := strings.Cut(p.bound, "TO")
		_, tsStr, _ := strings.Cut(rest, "'")
		tsStr, _, _ = strings.Cut(tsStr, "'")
		to, err := time.Parse(time.RFC3339, tsStr)
		if err != nil {
			continue
		}
		if to.Before(cutoff) {
			if _, err := pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, p.name)); err != nil {
				return err
			}
		}
	}
	return nil
}
