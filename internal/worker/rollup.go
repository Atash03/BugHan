package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Rollup maintenance (DESIGN.md §6 async jobs, §11 aggregates): recompute
// hourly per-transaction-name rollups from transactions_part. Percentiles are
// computed with percentile_cont at recompute time — no sketch bookkeeping at
// ingest — and rows are overwritten, so re-runs are idempotent and late
// arrivals inside the recomputed window are absorbed.

const (
	JobKindRollupMaintenance = "rollup_maintenance"

	// trailingHours recomputed on every pass (current hour + two elapsed)
	// absorbs late deliveries; older hours are only processed once, via the
	// watermark.
	trailingHours = 2 * time.Hour

	// maxBackfillHours bounds one pass when draining a backlog (e.g. after
	// downtime); the next pass picks up where this one left off.
	maxBackfillHours = 24
)

// RunRollupPass recomputes transaction rollups for the window between the
// stored watermark (or the earliest raw data) and now, advancing the
// watermark. Shared by the worker handler and tests.
func RunRollupPass(ctx context.Context, pool *pgxpool.Pool) error {
	now := time.Now().UTC()
	windowStart := now.Truncate(time.Hour).Add(-trailingHours)

	// Watermark: hours before it were fully processed. First run picks up
	// from the earliest raw hour (cheap min over the start_ts index) so a
	// pre-existing backlog is drained rather than skipped.
	var watermark *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT last_hour FROM rollup_state WHERE kind = $1`, JobKindRollupMaintenance).
		Scan(&watermark); err != nil && err.Error() != "no rows in result set" {
		return fmt.Errorf("read watermark: %w", err)
	}
	if watermark != nil && watermark.Before(windowStart) {
		windowStart = *watermark
	}

	// Bound one pass; anything left waits for the next.
	windowEnd := now.Truncate(time.Hour).Add(time.Hour)
	if max := windowStart.Add(maxBackfillHours * time.Hour); max.Before(windowEnd) {
		windowEnd = max
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO transaction_rollups (project_id, name, hour, count, failures, p50_ms, p95_ms, p99_ms)
		SELECT project_id, name, date_trunc('hour', start_ts),
			count(*),
			count(*) FILTER (WHERE status <> '' AND status <> 'ok'),
			percentile_cont(0.5)  WITHIN GROUP (ORDER BY duration_ms),
			percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms),
			percentile_cont(0.99) WITHIN GROUP (ORDER BY duration_ms)
		FROM transactions_part
		WHERE start_ts >= $1 AND start_ts < $2
		GROUP BY project_id, name, date_trunc('hour', start_ts)
		ON CONFLICT (project_id, name, hour) DO UPDATE SET
			count    = EXCLUDED.count,
			failures = EXCLUDED.failures,
			p50_ms   = EXCLUDED.p50_ms,
			p95_ms   = EXCLUDED.p95_ms,
			p99_ms   = EXCLUDED.p99_ms`,
		windowStart, windowEnd); err != nil {
		return fmt.Errorf("aggregate rollups: %w", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO rollup_state (kind, last_hour) VALUES ($1, $2)
		ON CONFLICT (kind) DO UPDATE SET last_hour = EXCLUDED.last_hour`,
		JobKindRollupMaintenance, windowEnd); err != nil {
		return fmt.Errorf("advance watermark: %w", err)
	}
	return nil
}

// RegisterRollupMaintenance wires the job into the worker and schedules its
// first pass. Each pass reschedules itself.
func (w *Worker) RegisterRollupMaintenance() error {
	w.Register(JobKindRollupMaintenance, func(ctx context.Context, _ json.RawMessage) error {
		if err := RunRollupPass(ctx, w.pool); err != nil {
			return err
		}
		return w.Enqueue(ctx, JobKindRollupMaintenance, nil, time.Now().UTC().Add(5*time.Minute))
	})
	return w.Enqueue(context.Background(), JobKindRollupMaintenance, nil, time.Now().UTC())
}
