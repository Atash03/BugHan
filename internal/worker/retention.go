package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Atash03/BugHan/internal/db"
)

// RetentionConfig carries the per-data-class retention windows (days) from
// the environment config (DESIGN.md §7 defaults).
type RetentionConfig struct {
	EventDays         int
	TransactionDays   int
	SessionDays       int
	SessionRollupDays int
	FeedbackDays      int
	ReleaseFileDays   int // files kept this long after a release's last event
}

// JobKindRetentionSweep is the recurring sweep job; it reschedules itself
// daily when it runs.
const JobKindRetentionSweep = "retention_sweep"

// RunRetentionSweep performs one sweep: drops old monthly partitions, deletes
// aged small-table rows, and keeps future partitions created. Issues and
// event rollups survive — they are the record after raw events age out.
func RunRetentionSweep(ctx context.Context, pool *pgxpool.Pool, rc RetentionConfig) error {
	now := time.Now().UTC()

	drops := []struct {
		table  string
		cutoff time.Time
	}{
		{"events_part", now.AddDate(0, 0, -rc.EventDays)},
		{"transactions_part", now.AddDate(0, 0, -rc.TransactionDays)},
		{"sessions_part", now.AddDate(0, 0, -rc.SessionDays)},
	}
	for _, d := range drops {
		if err := db.DropPartitions(ctx, pool, d.table, d.cutoff); err != nil {
			return fmt.Errorf("drop %s partitions: %w", d.table, err)
		}
	}

	smallDeletes := []struct {
		query  string
		cutoff time.Time
	}{
		{`DELETE FROM feedbacks WHERE received_at < $1`, now.AddDate(0, 0, -rc.FeedbackDays)},
		{`DELETE FROM issue_user_hashes WHERE first_seen < $1`, now.AddDate(0, 0, -rc.EventDays)},
		{`DELETE FROM session_rollups WHERE hour < $1`, now.AddDate(0, 0, -rc.SessionRollupDays)},
	}
	for _, d := range smallDeletes {
		if _, err := pool.Exec(ctx, d.query, d.cutoff); err != nil {
			return fmt.Errorf("retention delete: %w", err)
		}
	}

	// Keep the rolling partition window open for the next two months.
	if err := db.EnsurePartitions(ctx, pool, []time.Time{now, now.AddDate(0, 1, 0)}); err != nil {
		return fmt.Errorf("ensure partitions: %w", err)
	}
	return nil
}

// RegisterRetention wires the sweep into the worker and schedules its first
// run; each run re-enqueues the next day.
func (w *Worker) RegisterRetention(rc RetentionConfig) error {
	w.Register(JobKindRetentionSweep, func(ctx context.Context, _ json.RawMessage) error {
		if err := RunRetentionSweep(ctx, w.pool, rc); err != nil {
			return err
		}
		return w.Enqueue(ctx, JobKindRetentionSweep, nil, time.Now().UTC().Add(24*time.Hour))
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return w.Enqueue(ctx, JobKindRetentionSweep, nil, time.Now().UTC())
}
