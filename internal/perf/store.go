package perf

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Atash03/BugHan/internal/hll"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Result types for the performance plane (DESIGN.md §11). Percentiles for
// summary windows are recomputed from raw rows (exact, and raw retention is
// 30d); hourly series and long-lived charts read transaction_rollups.

// TransactionSummaryRow aggregates one transaction name over a window.
type TransactionSummaryRow struct {
	Name         string  `json:"name"`
	Count        int64   `json:"count"`
	Failures     int64   `json:"failures"`
	P50          float64 `json:"p50"`
	P95          float64 `json:"p95"`
	P99          float64 `json:"p99"`
	RecentlySlow bool    `json:"recently_slow"`
}

// TransactionSummary aggregates per-name counts, failure counts, and
// percentiles over [since, until], flagging names whose last-hour p95
// exceeds twice their trailing-7-day baseline.
func TransactionSummary(ctx context.Context, pool *pgxpool.Pool, projectID string, since, until time.Time) ([]TransactionSummaryRow, error) {
	rows, err := pool.Query(ctx, `
		SELECT name, count(*),
			count(*) FILTER (WHERE status <> '' AND status <> 'ok'),
			percentile_cont(0.5)  WITHIN GROUP (ORDER BY duration_ms),
			percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms),
			percentile_cont(0.99) WITHIN GROUP (ORDER BY duration_ms),
			-- recently slow: trailing-hour p95 vs 7-day baseline (both
			-- hour-aligned, excluding the trailing hour from the baseline)
			coalesce((SELECT percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms)
				FROM transactions_part t2
				WHERE t2.project_id = t.project_id AND t2.name = t.name
				  AND t2.start_ts >= date_trunc('hour', now()) - interval '1 hour'
				  AND t2.start_ts < now()), 0) >
			2 * coalesce((SELECT percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms)
				FROM transactions_part t3
				WHERE t3.project_id = t.project_id AND t3.name = t.name
				  AND t3.start_ts >= now() - interval '7 days'
				  AND t3.start_ts < date_trunc('hour', now()) - interval '1 hour'), 0)
		FROM transactions_part t
		WHERE project_id = $1 AND start_ts >= $2 AND start_ts < $3
		GROUP BY project_id, name`, projectID, since, until)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TransactionSummaryRow{}
	for rows.Next() {
		var r TransactionSummaryRow
		if err := rows.Scan(&r.Name, &r.Count, &r.Failures, &r.P50, &r.P95, &r.P99, &r.RecentlySlow); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// HourlyBucket is one hour of a transaction's rollup series.
type HourlyBucket struct {
	Hour     time.Time `json:"hour"`
	Count    int64     `json:"count"`
	Failures int64     `json:"failures"`
	P50      float64   `json:"p50"`
	P95      float64   `json:"p95"`
	P99      float64   `json:"p99"`
}

// HistogramBucket is one linear bucket of the duration histogram.
type HistogramBucket struct {
	FromMS float64 `json:"from_ms"`
	ToMS   float64 `json:"to_ms"`
	Count  int64   `json:"count"`
}

// TransactionEvent is one raw transaction in the recent-events list.
type TransactionEvent struct {
	ID         string             `json:"id"`
	Start      time.Time          `json:"start_ts"`
	DurationMS float64            `json:"duration_ms"`
	Status     string             `json:"status"`
	TraceID    string             `json:"trace_id"`
	Vitals     map[string]float64 `json:"vitals,omitempty"`
}

// TransactionDetail powers the transaction page: window aggregates, the
// hourly rollup series, recent raw events, and a duration histogram.
type TransactionDetailView struct {
	Name      string             `json:"name"`
	Count     int64              `json:"count"`
	Failures  int64              `json:"failures"`
	P50       float64            `json:"p50"`
	P95       float64            `json:"p95"`
	P99       float64            `json:"p99"`
	Hourly    []HourlyBucket     `json:"hourly"`
	Recent    []TransactionEvent `json:"recent"`
	Histogram []HistogramBucket  `json:"histogram"`
}

// TransactionDetail assembles the detail view for one transaction name.
func TransactionDetail(ctx context.Context, pool *pgxpool.Pool, projectID, name string, since, until time.Time) (*TransactionDetailView, error) {
	d := &TransactionDetailView{Name: name}

	if err := pool.QueryRow(ctx, `
		SELECT count(*),
			count(*) FILTER (WHERE status <> '' AND status <> 'ok'),
			percentile_cont(0.5)  WITHIN GROUP (ORDER BY duration_ms),
			percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms),
			percentile_cont(0.99) WITHIN GROUP (ORDER BY duration_ms)
		FROM transactions_part
		WHERE project_id = $1 AND name = $2 AND start_ts >= $3 AND start_ts < $4`,
		projectID, name, since, until).
		Scan(&d.Count, &d.Failures, &d.P50, &d.P95, &d.P99); err != nil {
		return nil, err
	}

	rows, err := pool.Query(ctx, `
		SELECT hour, count, failures, p50_ms, p95_ms, p99_ms
		FROM transaction_rollups
		WHERE project_id = $1 AND name = $2 AND hour >= $3 AND hour < $4
		ORDER BY hour`, projectID, name, since.Truncate(time.Hour), until)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	d.Hourly = []HourlyBucket{}
	for rows.Next() {
		var b HourlyBucket
		if err := rows.Scan(&b.Hour, &b.Count, &b.Failures, &b.P50, &b.P95, &b.P99); err != nil {
			return nil, err
		}
		d.Hourly = append(d.Hourly, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if err := d.loadRecent(ctx, pool, projectID, since, until); err != nil {
		return nil, err
	}
	if err := d.loadHistogram(ctx, pool, projectID, since, until); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *TransactionDetailView) loadRecent(ctx context.Context, pool *pgxpool.Pool, projectID string, since, until time.Time) error {
	rows, err := pool.Query(ctx, `
		SELECT id::text, start_ts, duration_ms, status, coalesce(trace_id::text, ''), measurements
		FROM transactions_part
		WHERE project_id = $1 AND name = $2 AND start_ts >= $3 AND start_ts < $4
		ORDER BY start_ts DESC LIMIT 50`,
		projectID, d.Name, since, until)
	if err != nil {
		return err
	}
	defer rows.Close()
	d.Recent = []TransactionEvent{}
	for rows.Next() {
		var e TransactionEvent
		var measurements json.RawMessage
		var traceID string
		if err := rows.Scan(&e.ID, &e.Start, &e.DurationMS, &e.Status, &traceID, &measurements); err != nil {
			return err
		}
		e.TraceID = traceID
		e.Vitals = ExtractWebVitals(measurements)
		d.Recent = append(d.Recent, e)
	}
	return rows.Err()
}

func (d *TransactionDetailView) loadHistogram(ctx context.Context, pool *pgxpool.Pool, projectID string, since, until time.Time) error {
	if d.Count == 0 || d.P99 == 0 {
		d.Histogram = []HistogramBucket{}
		return nil
	}
	const buckets = 20
	rows, err := pool.Query(ctx, `
		SELECT width_bucket(duration_ms, 0, $5, $6) AS b, count(*)
		FROM transactions_part
		WHERE project_id = $1 AND name = $2 AND start_ts >= $3 AND start_ts < $4
		GROUP BY b ORDER BY b`,
		projectID, d.Name, since, until, d.P99, buckets)
	if err != nil {
		return err
	}
	defer rows.Close()
	d.Histogram = []HistogramBucket{}
	width := d.P99 / buckets
	for rows.Next() {
		var b int64
		var count int64
		if err := rows.Scan(&b, &count); err != nil {
			return err
		}
		d.Histogram = append(d.Histogram, HistogramBucket{
			FromMS: float64(b-1) * width,
			ToMS:   float64(b) * width,
			Count:  count,
		})
	}
	return rows.Err()
}

// TraceSpan is one span extracted from a transaction payload.
type TraceSpan struct {
	SpanID       string    `json:"span_id"`
	ParentSpanID string    `json:"parent_span_id"`
	Op           string    `json:"op"`
	Description  string    `json:"description"`
	Status       string    `json:"status"`
	Start        time.Time `json:"start_ts"`
	End          time.Time `json:"end_ts"`
	DurationMS   float64   `json:"duration_ms"`
}

// TraceTransaction is one raw transaction row in a trace, spans included.
type TraceTransaction struct {
	ID          string             `json:"id"`
	TraceID     string             `json:"trace_id"`
	Name        string             `json:"name"`
	Status      string             `json:"status"`
	Environment string             `json:"environment"`
	Release     string             `json:"release"`
	Start       time.Time          `json:"start_ts"`
	DurationMS  float64            `json:"duration_ms"`
	Vitals      map[string]float64 `json:"vitals,omitempty"`
	Spans       []TraceSpan        `json:"spans"`
}

// TraceError is an error event sharing the trace id.
type TraceError struct {
	ID        string    `json:"id"`
	IssueID   string    `json:"issue_id"`
	Title     string    `json:"title"`
	Level     string    `json:"level"`
	Culprit   string    `json:"culprit"`
	Timestamp time.Time `json:"timestamp"`
}

// Trace is the trace view payload: transactions ordered by start with their
// payload spans, plus linked error events.
type Trace struct {
	TraceID      string             `json:"trace_id"`
	Transactions []TraceTransaction `json:"transactions"`
	Errors       []TraceError       `json:"errors"`
}

// TraceByID loads one trace by id (DESIGN.md §11): transactions sharing the
// trace_id (spans parsed from the payload) and error events on the same id.
func TraceByID(ctx context.Context, pool *pgxpool.Pool, projectID, traceID string) (*Trace, error) {
	tid, err := uuid.Parse(traceID)
	if err != nil {
		return nil, fmt.Errorf("bad trace id %q: %w", traceID, err)
	}
	trace := &Trace{TraceID: traceID, Transactions: []TraceTransaction{}, Errors: []TraceError{}}

	rows, err := pool.Query(ctx, `
		SELECT id::text, coalesce(trace_id::text, ''), name, status, environment, release,
		       start_ts, duration_ms, measurements, payload
		FROM transactions_part
		WHERE project_id = $1 AND trace_id = $2
		ORDER BY start_ts`, projectID, tid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var tt TraceTransaction
		var measurements, payload json.RawMessage
		if err := rows.Scan(&tt.ID, &tt.TraceID, &tt.Name, &tt.Status, &tt.Environment, &tt.Release,
			&tt.Start, &tt.DurationMS, &measurements, &payload); err != nil {
			return nil, err
		}
		tt.Vitals = ExtractWebVitals(measurements)
		tt.Spans = parseSpans(payload)
		trace.Transactions = append(trace.Transactions, tt)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	erows, err := pool.Query(ctx, `
		SELECT id::text, coalesce(issue_id::text, ''), title, level, culprit, timestamp
		FROM events_part
		WHERE project_id = $1 AND trace_id = $2
		ORDER BY timestamp`, projectID, tid)
	if err != nil {
		return nil, err
	}
	defer erows.Close()
	for erows.Next() {
		var te TraceError
		if err := erows.Scan(&te.ID, &te.IssueID, &te.Title, &te.Level, &te.Culprit, &te.Timestamp); err != nil {
			return nil, err
		}
		trace.Errors = append(trace.Errors, te)
	}
	return trace, erows.Err()
}

type rawSpan struct {
	SpanID         string          `json:"span_id"`
	ParentSpanID   string          `json:"parent_span_id"`
	Op             string          `json:"op"`
	Description    string          `json:"description"`
	Status         string          `json:"status"`
	StartTimestamp json.RawMessage `json:"start_timestamp"`
	Timestamp      json.RawMessage `json:"timestamp"`
}

// parseSpans extracts the payload's spans array; transactions without spans
// (or with unparseable ones) yield an empty list, never an error.
func parseSpans(payload json.RawMessage) []TraceSpan {
	out := []TraceSpan{}
	if len(payload) == 0 {
		return out
	}
	var p struct {
		Spans []rawSpan `json:"spans"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return out
	}
	for _, s := range p.Spans {
		span := TraceSpan{
			SpanID:       s.SpanID,
			ParentSpanID: s.ParentSpanID,
			Op:           s.Op,
			Description:  s.Description,
			Status:       s.Status,
		}
		span.Start = parseFlexibleTime(s.StartTimestamp)
		span.End = parseFlexibleTime(s.Timestamp)
		if !span.Start.IsZero() && !span.End.IsZero() {
			span.DurationMS = span.End.Sub(span.Start).Seconds() * 1000
		}
		out = append(out, span)
	}
	return out
}

// parseFlexibleTime accepts unix seconds (SDK form) or RFC 3339; a zero time
// marks "not provided".
func parseFlexibleTime(raw json.RawMessage) time.Time {
	if len(raw) == 0 {
		return time.Time{}
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return time.UnixMilli(int64(f * 1000)).UTC()
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// ReleaseHealthRow aggregates one release's session health over a window.
type ReleaseHealthRow struct {
	Release           string   `json:"release"`
	Total             int64    `json:"total"`
	Crashed           int64    `json:"crashed"`
	Abnormal          int64    `json:"abnormal"`
	Errored           int64    `json:"errored"`
	CrashFreeSessions float64  `json:"crash_free_sessions"`
	CrashFreeUsers    *float64 `json:"crash_free_users"`
	UniqueUsers       uint64   `json:"unique_users"`
}

// ReleaseHealth folds session rollups across the window: crash-free session
// rate is 1 − crashed/total; crash-free user rate unions the HLL did
// sketches (all vs crashed) per DESIGN.md §9.
func ReleaseHealth(ctx context.Context, pool *pgxpool.Pool, projectID string, since, until time.Time) ([]ReleaseHealthRow, error) {
	type pending struct {
		row       ReleaseHealthRow
		allDids   *hll.Sketch
		crashDids *hll.Sketch
	}
	sketches := map[string]*pending{}
	srows, err := pool.Query(ctx, `
		SELECT release, total, crashed, abnormal, errored, distinct_did, crashed_did
		FROM session_rollups
		WHERE project_id = $1 AND hour >= $2 AND hour < $3
		ORDER BY release, hour`, projectID, since.Truncate(time.Hour), until)
	if err != nil {
		return nil, err
	}
	defer srows.Close()
	for srows.Next() {
		var release string
		var total, crashed, abnormal, errored int64
		var allBlob, crashBlob []byte
		if err := srows.Scan(&release, &total, &crashed, &abnormal, &errored, &allBlob, &crashBlob); err != nil {
			return nil, err
		}
		p, ok := sketches[release]
		if !ok {
			p = &pending{row: ReleaseHealthRow{Release: release}}
			sketches[release] = p
		}
		p.row.Total += total
		p.row.Crashed += crashed
		p.row.Abnormal += abnormal
		p.row.Errored += errored
		for i, blob := range [][]byte{allBlob, crashBlob} {
			if len(blob) == 0 {
				continue
			}
			sk, err := hll.UnmarshalBinary(blob)
			if err != nil {
				return nil, fmt.Errorf("release %q sketch: %w", release, err)
			}
			if i == 0 {
				if p.allDids == nil {
					p.allDids = sk
				} else {
					p.allDids.Merge(sk)
				}
			} else {
				if p.crashDids == nil {
					p.crashDids = sk
				} else {
					p.crashDids.Merge(sk)
				}
			}
		}
	}
	if err := srows.Err(); err != nil {
		return nil, err
	}

	out := []ReleaseHealthRow{}
	for _, p := range sketches {
		r := p.row
		if r.Total > 0 {
			r.CrashFreeSessions = 1 - float64(r.Crashed)/float64(r.Total)
		}
		if p.allDids != nil {
			r.UniqueUsers = p.allDids.Estimate()
		}
		if p.allDids != nil && p.crashDids != nil && r.UniqueUsers > 0 {
			rate := 1 - float64(p.crashDids.Estimate())/float64(r.UniqueUsers)
			r.CrashFreeUsers = &rate
		}
		out = append(out, r)
	}
	return out, nil
}
