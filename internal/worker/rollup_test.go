package worker

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/Atash03/BugHan/internal/testdb"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The rollup-maintenance pass (DESIGN.md §11) aggregates raw transactions
// into hourly per-name rollups: count, failures, p50/p95/p99. Behavior under
// test: correct aggregates, idempotent re-runs, late arrivals inside the
// trailing window absorbed, watermark advancing.

type txnRow struct {
	start    time.Time
	name     string
	status   string
	duration float64 // ms
}

func seedTransactions(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID string, rows []txnRow) {
	t.Helper()
	for i, r := range rows {
		if _, err := pool.Exec(ctx, `
			INSERT INTO transactions_part
				(id, project_id, timestamp, start_ts, duration_ms, trace_id, span_id,
				 name, source, status, environment, release, dist, measurements, payload)
			VALUES ($1,$2,$3,$4,$5,NULL,'',$6,'view',$7,'production','','','{}','{}')`,
			uuid.NewString(), projectID, r.start.Add(30*time.Second), r.start,
			r.duration, r.name, r.status); err != nil {
			t.Fatalf("seed txn %d: %v", i, err)
		}
	}
}

func TestRollupPassAggregatesAndIsIdempotent(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	projectID := seedWorkerProject(t, ctx, pool)

	hour1 := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	hour2 := hour1.Add(time.Hour)
	base := hour1.Add(10 * time.Minute)
	seedTransactions(t, ctx, pool, projectID, []txnRow{
		// "GET /list" in hour1: durations 10,20,30,40 (ms), one failure.
		{base, "GET /list", "ok", 10},
		{base.Add(time.Second), "GET /list", "ok", 20},
		{base.Add(2 * time.Second), "GET /list", "internal_error", 30},
		{base.Add(3 * time.Second), "GET /list", "ok", 40},
		// hour2, same name.
		{hour2.Add(5 * time.Minute), "GET /list", "ok", 100},
		// hour2, other name.
		{hour2.Add(6 * time.Minute), "GET /slow", "ok", 250},
	})

	if err := RunRollupPass(ctx, pool); err != nil {
		t.Fatalf("RunRollupPass: %v", err)
	}

	type rollup struct {
		name            string
		count, failures int64
		p50, p95, p99   float64
	}
	var got []rollup
	rows, err := pool.Query(ctx, `
		SELECT name, count, failures, p50_ms, p95_ms, p99_ms FROM transaction_rollups
		WHERE project_id = $1 ORDER BY name, hour`, projectID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var r rollup
		if err := rows.Scan(&r.name, &r.count, &r.failures, &r.p50, &r.p95, &r.p99); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if len(got) != 3 { // /list×hour1, /list×hour2, /slow×hour2
		t.Fatalf("got %d rollup rows %+v, want 3", len(got), got)
	}
	// percentile_cont over [10,20,30,40]: p50=25, p95=38.5, p99=39.7.
	l1 := got[0]
	if l1.name != "GET /list" || l1.count != 4 || l1.failures != 1 {
		t.Fatalf("hour1 /list: %+v", l1)
	}
	for name, want := range map[string]float64{"p50": 25, "p95": 38.5, "p99": 39.7} {
		have := map[string]float64{"p50": l1.p50, "p95": l1.p95, "p99": l1.p99}[name]
		if math.Abs(have-want) > 0.01 {
			t.Fatalf("hour1 /list %s = %v, want %v", name, have, want)
		}
	}
	if got[1].name == "GET /list" && (got[1].count != 1 || got[1].p50 != 100) {
		t.Fatalf("hour2 /list: %+v", got[1])
	}
	if got[2].name == "GET /slow" && (got[2].count != 1 || got[2].p50 != 250) {
		t.Fatalf("hour2 /slow: %+v", got[2])
	}

	// Re-running must not double-count.
	if err := RunRollupPass(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := pool.QueryRow(ctx, `SELECT count FROM transaction_rollups
		WHERE project_id=$1 AND name='GET /list' AND hour=$2`, projectID, hour1).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 4 {
		t.Fatalf("count after re-run = %d, want 4", count)
	}

	// A late arrival inside the trailing window is absorbed on the next pass.
	seedTransactions(t, ctx, pool, projectID, []txnRow{
		{hour1.Add(20 * time.Minute), "GET /list", "ok", 50},
	})
	if err := RunRollupPass(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var p50 float64
	if err := pool.QueryRow(ctx, `SELECT p50_ms FROM transaction_rollups
		WHERE project_id=$1 AND name='GET /list' AND hour=$2`, projectID, hour1).Scan(&p50); err != nil {
		t.Fatal(err)
	}
	if math.Abs(p50-30) > 0.01 { // percentile_cont([10,20,30,40,50]) = 30
		t.Fatalf("p50 after late arrival = %v, want 30", p50)
	}

	// The watermark advanced to the current hour.
	var lastHour time.Time
	if err := pool.QueryRow(ctx, `SELECT last_hour FROM rollup_state WHERE kind='rollup_maintenance'`).
		Scan(&lastHour); err != nil {
		t.Fatalf("watermark: %v", err)
	}
	if lastHour.Before(hour2) {
		t.Fatalf("watermark %v did not advance past %v", lastHour, hour2)
	}
}
