package perf

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/Atash03/BugHan/internal/hll"
	"github.com/Atash03/BugHan/internal/testdb"
	"github.com/Atash03/BugHan/internal/worker"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Behavior under test: the performance queries the REST endpoints serve
// (DESIGN.md §11) — window summary with recently-slow detection, transaction
// detail (hourly series + recent events + histogram), trace-by-id (spans read
// from the payload, error events linked), and release health (crash-free
// session/user rates from rollups + HLL sketches).

func seedProjectRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	orgID := uuid.NewString()
	projectID := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO organizations (id, name, slug) VALUES ($1, 'Org', $2)`,
		orgID, "org-"+projectID[:8]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO projects (id, org_id, name, slug) VALUES ($1, $2, 'Proj', $3)`,
		projectID, orgID, "proj-"+projectID[:8]); err != nil {
		t.Fatal(err)
	}
	return projectID
}

// seedTxn inserts one raw transaction row.
func seedTxn(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID string, start time.Time, name, status string, durationMS float64, payload string) {
	t.Helper()
	if payload == "" {
		payload = `{}`
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO transactions_part
			(id, project_id, timestamp, start_ts, duration_ms, trace_id, span_id,
			 name, source, status, environment, release, dist, measurements, payload)
		VALUES ($1,$2,$3,$4,$5,NULL,'',$6,'view',$7,'production','','','{}',$8)`,
		uuid.NewString(), projectID, start.Add(30*time.Second), start,
		durationMS, name, status, payload); err != nil {
		t.Fatalf("seed txn: %v", err)
	}
}

func TestTransactionSummaryAggregatesWindowAndFlagsRecentlySlow(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	projectID := seedProjectRow(t, ctx, pool)

	hourAgo := time.Now().UTC().Truncate(time.Hour).Add(-30 * time.Minute)
	daysAgo := func(days int) time.Time {
		return time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	}
	// "steady": ~100ms always → not recently slow.
	for i := 0; i < 5; i++ {
		seedTxn(t, ctx, pool, projectID, daysAgo(3).Add(time.Duration(i)*time.Minute), "steady", "ok", 100, "")
		seedTxn(t, ctx, pool, projectID, hourAgo.Add(time.Duration(i)*time.Second), "steady", "ok", 100, "")
	}
	// "slow": baseline 100ms over the week, 500ms in the last hour.
	for i := 0; i < 5; i++ {
		seedTxn(t, ctx, pool, projectID, daysAgo(3).Add(time.Duration(i)*time.Minute), "slow", "ok", 100, "")
	}
	for i := 0; i < 5; i++ {
		seedTxn(t, ctx, pool, projectID, hourAgo.Add(time.Duration(i)*time.Second), "slow", "ok", 500, "")
	}

	rows, err := TransactionSummary(ctx, pool, projectID, daysAgo(7), time.Now().UTC())
	if err != nil {
		t.Fatalf("TransactionSummary: %v", err)
	}
	byName := map[string]TransactionSummaryRow{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	slow, ok := byName["slow"]
	if !ok {
		t.Fatalf("summary missing 'slow': %+v", rows)
	}
	if slow.Count != 10 || slow.Failures != 0 {
		t.Fatalf("slow count=%d failures=%d, want 10/0", slow.Count, slow.Failures)
	}
	if slow.P50 != 300 || slow.P95 != 500 {
		t.Fatalf("slow p50=%v p95=%v (window blend of 100s and 500s)", slow.P50, slow.P95)
	}
	if !slow.RecentlySlow {
		t.Fatal("slow not flagged: last-hour p95 500 vs baseline ~100")
	}
	steady, ok := byName["steady"]
	if !ok {
		t.Fatalf("summary missing 'steady': %+v", rows)
	}
	if steady.RecentlySlow {
		t.Fatal("steady wrongly flagged recently-slow")
	}
	// A failure status counts toward the failure rate.
	seedTxn(t, ctx, pool, projectID, hourAgo, "failing", "internal_error", 100, "")
	rows, err = TransactionSummary(ctx, pool, projectID, daysAgo(7), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Name == "failing" && (r.Count != 1 || r.Failures != 1) {
			t.Fatalf("failing count=%d failures=%d, want 1/1", r.Count, r.Failures)
		}
	}
}

func TestTransactionDetailHourlyRecentAndHistogram(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	projectID := seedProjectRow(t, ctx, pool)

	hour1 := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	for i, d := range []float64{10, 20, 30, 40} {
		seedTxn(t, ctx, pool, projectID, hour1.Add(time.Duration(i)*time.Second), "GET /list", "ok", d, "")
	}

	// The hourly series reads transaction_rollups, filled by the maintenance
	// pass.
	if err := worker.RunRollupPass(ctx, pool); err != nil {
		t.Fatalf("RunRollupPass: %v", err)
	}

	detail, err := TransactionDetail(ctx, pool, projectID, "GET /list",
		hour1.Add(-time.Hour), time.Now().UTC())
	if err != nil {
		t.Fatalf("TransactionDetail: %v", err)
	}
	if detail.Count != 4 || detail.P50 != 25 {
		t.Fatalf("detail count=%d p50=%v, want 4/25", detail.Count, detail.P50)
	}
	if len(detail.Hourly) != 1 || detail.Hourly[0].Count != 4 {
		t.Fatalf("hourly series: %+v", detail.Hourly)
	}
	if len(detail.Recent) != 4 || detail.Recent[0].DurationMS != 40 {
		t.Fatalf("recent events (newest first): %+v", detail.Recent)
	}
	// Histogram buckets cover all events.
	total := int64(0)
	for _, b := range detail.Histogram {
		total += b.Count
	}
	if total != 4 {
		t.Fatalf("histogram covers %d events, want 4: %+v", total, detail.Histogram)
	}
}

func TestTraceByIDReturnsTransactionsWithSpansAndErrors(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	projectID := seedProjectRow(t, ctx, pool)
	traceID := uuid.NewString()
	start := time.Now().UTC().Add(-time.Hour)

	// Root transaction with a child span inside the payload; spans stay in
	// the payload (DESIGN.md §7) — the trace view reads one row.
	rootPayload := `{
		"spans": [{
			"span_id": "abcdef0123456789",
			"parent_span_id": "0000000000000000",
			"op": "fetch",
			"description": "GET /api/todos",
			"start_timestamp": ` + unixSeconds(start.Add(10*time.Millisecond)) + `,
			"timestamp": ` + unixSeconds(start.Add(60*time.Millisecond)) + `,
			"status": "ok"
		}]
	}`
	seedTraceTxn(t, ctx, pool, projectID, traceID, start, "page-load", 200, rootPayload)
	seedTraceTxn(t, ctx, pool, projectID, traceID, start.Add(50*time.Millisecond), "navigation", 100, `{}`)

	// An error event sharing the trace id shows up as a red tick.
	if _, err := pool.Exec(ctx,
		`INSERT INTO issues (id, project_id, fingerprint, first_seen, last_seen)
		 VALUES ($1,$2,'fp',$3,$3)`, uuid.NewString(), projectID, start); err != nil {
		t.Fatal(err)
	}
	var issueID string
	if err := pool.QueryRow(ctx, `SELECT id FROM issues WHERE project_id=$1`, projectID).Scan(&issueID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO events_part (id, project_id, issue_id, timestamp, title, level, trace_id, payload)
		VALUES ($1,$2,$3,$4,'TypeError: boom','error',$5,'{}')`,
		uuid.NewString(), projectID, issueID, start.Add(30*time.Millisecond), traceID); err != nil {
		t.Fatal(err)
	}

	trace, err := TraceByID(ctx, pool, projectID, traceID)
	if err != nil {
		t.Fatalf("TraceByID: %v", err)
	}
	if len(trace.Transactions) != 2 {
		t.Fatalf("got %d transactions, want 2", len(trace.Transactions))
	}
	// Ordered by start: root first, with its span parsed from the payload.
	root := trace.Transactions[0]
	if root.Name != "page-load" || len(root.Spans) != 1 {
		t.Fatalf("root transaction: %+v spans=%d", root, len(root.Spans))
	}
	span := root.Spans[0]
	if span.Op != "fetch" || span.Description != "GET /api/todos" {
		t.Fatalf("span parsed wrong: %+v", span)
	}
	if d := span.End.Sub(span.Start).Milliseconds(); d != 50 {
		t.Fatalf("span duration = %dms, want 50", d)
	}
	if len(trace.Errors) != 1 || trace.Errors[0].Title != "TypeError: boom" || trace.Errors[0].IssueID != issueID {
		t.Fatalf("linked errors: %+v", trace.Errors)
	}
}

func seedTraceTxn(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID, traceID string, start time.Time, name string, durationMS float64, payload string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO transactions_part
			(id, project_id, timestamp, start_ts, duration_ms, trace_id, span_id,
			 name, source, status, environment, release, dist, measurements, payload)
		VALUES ($1,$2,$3,$4,$5,$6,'',$7,'view','ok','production','','','{}',$8)`,
		uuid.NewString(), projectID, start.Add(time.Duration(durationMS)*time.Millisecond), start,
		durationMS, traceID, name, payload); err != nil {
		t.Fatalf("seed trace txn: %v", err)
	}
}

// unixSeconds renders a time the way the SDK writes span timestamps: epoch
// seconds as a JSON number.
func unixSeconds(t time.Time) string {
	return strconv.FormatFloat(float64(t.UnixNano())/1e9, 'f', 3, 64)
}

func TestReleaseHealthCrashFreeRates(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	projectID := seedProjectRow(t, ctx, pool)

	hour := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	// web@1.0.0: 10 sessions, 2 crashed; 5 distinct dids, 1 crashed did.
	all := hll.New()
	for i := 0; i < 5; i++ {
		all.Insert([]byte{byte(i)})
	}
	crashed := hll.New()
	crashed.Insert([]byte{byte(0)})
	allBlob, _ := all.MarshalBinary()
	crashedBlob, _ := crashed.MarshalBinary()
	if _, err := pool.Exec(ctx, `
		INSERT INTO session_rollups (project_id, release, environment, hour,
			total, crashed, abnormal, errored, exited, duration_sum, distinct_did, crashed_did)
		VALUES ($1,'web@1.0.0','production',$2,10,2,0,3,8,0,$3,$4)`,
		projectID, hour, allBlob, crashedBlob); err != nil {
		t.Fatal(err)
	}

	rows, err := ReleaseHealth(ctx, pool, projectID, hour.Add(-time.Hour), time.Now().UTC())
	if err != nil {
		t.Fatalf("ReleaseHealth: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d releases, want 1", len(rows))
	}
	r := rows[0]
	if r.Release != "web@1.0.0" || r.Total != 10 || r.Crashed != 2 {
		t.Fatalf("release row: %+v", r)
	}
	if r.CrashFreeSessions != 0.8 {
		t.Fatalf("crash-free sessions = %v, want 0.8", r.CrashFreeSessions)
	}
	if r.CrashFreeUsers == nil || *r.CrashFreeUsers != 0.8 {
		t.Fatalf("crash-free users = %v, want 0.8 (1 crashed of 5 dids)", r.CrashFreeUsers)
	}
}
