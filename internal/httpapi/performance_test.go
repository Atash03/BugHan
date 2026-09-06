package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Atash03/BugHan/internal/worker"
	"github.com/google/uuid"
)

// The performance REST surface (DESIGN.md §11, ticket #40): window summary,
// transaction detail, trace by id, and release health — project-scoped,
// member-visible, served from the perf query layer.

func seedPerfProject(t *testing.T, s *Server) (projectID, projectSlug string, sess jar) {
	t.Helper()
	sess = setupAccount(t, s, "neo@acme.dev")
	csrf := extractCSRF(s.get(t, "/auth/landing/", sess, nil).Body.String())
	rec := s.apiCall(t, "POST", "/api/0/organizations/acme-corp/projects/",
		`{"name":"Web"}`, sess, map[string]string{"X-CSRF-Token": csrf})
	if rec.Code != 201 {
		t.Fatalf("create project = %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	return created.ID, created.Slug, sess
}

func TestPerformanceEndpointsServeQueryLayer(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	projectID, slug, sess := seedPerfProject(t, s)

	// One trace with a transaction + error event, and enough raw rows for
	// summary/detail; the rollup pass fills the hourly series.
	traceID := uuid.NewString()
	hour1 := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	for i, d := range []float64{10, 20, 30, 40} {
		start := hour1.Add(time.Duration(i) * time.Second)
		// Only the first transaction belongs to the trace; the rest are
		// untraced traffic for summary/detail.
		var tr any
		if i == 0 {
			tr = traceID
		}
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO transactions_part (id, project_id, timestamp, start_ts, duration_ms,
				trace_id, span_id, name, source, status, environment, release, dist, measurements, payload)
			VALUES ($1,$2,$3,$4,$5,$6,'','GET /list','view','ok','production','web@1.0.0','','{}',
				'{"spans":[{"span_id":"aaa","op":"fetch","description":"GET /x","start_timestamp":1,"timestamp":1.01}]}')`,
			uuid.NewString(), projectID, start, start, d, tr); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO events_part (id, project_id, timestamp, title, level, trace_id, payload)
		VALUES ($1,$2,$3,'TypeError: boom','error',$4,'{}')`,
		uuid.NewString(), projectID, hour1.Add(time.Minute), traceID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO session_rollups (project_id, release, environment, hour, total, crashed)
		VALUES ($1,'web@1.0.0','production',$2,10,2)`, projectID, hour1); err != nil {
		t.Fatal(err)
	}
	if err := worker.RunRollupPass(ctx, s.pool); err != nil {
		t.Fatalf("rollup pass: %v", err)
	}

	base := fmt.Sprintf("/api/0/projects/acme-corp/%s", slug)

	// Summary: per-name aggregates from the query layer.
	rec := s.get(t, base+"/performance/summary/?statsPeriod=24h", sess, nil)
	if rec.Code != 200 {
		t.Fatalf("summary = %d: %s", rec.Code, rec.Body.String())
	}
	var summary []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if len(summary) != 1 || summary[0]["name"] != "GET /list" {
		t.Fatalf("summary rows: %v", summary)
	}
	if summary[0]["count"].(float64) != 4 {
		t.Fatalf("summary count = %v, want 4", summary[0]["count"])
	}

	// Transaction detail: aggregates + hourly + recent + histogram.
	rec = s.get(t, base+"/performance/transaction/?name=GET+%2Flist&statsPeriod=24h", sess, nil)
	if rec.Code != 200 {
		t.Fatalf("transaction detail = %d: %s", rec.Code, rec.Body.String())
	}
	var detail struct {
		Name      string           `json:"name"`
		Count     int64            `json:"count"`
		P50       float64          `json:"p50"`
		Hourly    []map[string]any `json:"hourly"`
		Recent    []map[string]any `json:"recent"`
		Histogram []map[string]any `json:"histogram"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Name != "GET /list" || detail.Count != 4 || detail.P50 != 25 {
		t.Fatalf("detail head: %+v", detail)
	}
	if len(detail.Hourly) == 0 || len(detail.Recent) != 4 || len(detail.Histogram) == 0 {
		t.Fatalf("detail series: hourly=%d recent=%d hist=%d",
			len(detail.Hourly), len(detail.Recent), len(detail.Histogram))
	}

	// Trace by id: transactions (spans from payload) + linked errors.
	rec = s.get(t, base+"/traces/"+traceID+"/", sess, nil)
	if rec.Code != 200 {
		t.Fatalf("trace = %d: %s", rec.Code, rec.Body.String())
	}
	var trace struct {
		TraceID      string           `json:"trace_id"`
		Transactions []map[string]any `json:"transactions"`
		Errors       []map[string]any `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &trace); err != nil {
		t.Fatal(err)
	}
	if trace.TraceID != traceID || len(trace.Transactions) != 1 || len(trace.Errors) != 1 {
		t.Fatalf("trace: %+v", trace)
	}
	spans, _ := trace.Transactions[0]["spans"].([]any)
	if len(spans) != 1 {
		t.Fatalf("trace transaction spans: %v", trace.Transactions[0]["spans"])
	}

	// Release health: crash-free session rate from the rollup row.
	rec = s.get(t, base+"/releases-health/?statsPeriod=24h", sess, nil)
	if rec.Code != 200 {
		t.Fatalf("releases-health = %d: %s", rec.Code, rec.Body.String())
	}
	var health []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if len(health) != 1 || health[0]["release"] != "web@1.0.0" {
		t.Fatalf("health rows: %v", health)
	}
	if got := health[0]["crash_free_sessions"].(float64); got != 0.8 {
		t.Fatalf("crash_free_sessions = %v, want 0.8", got)
	}
}

func TestPerformanceEndpointsRequireMembership(t *testing.T) {
	s := newTestServer(t)
	projectID, slug, sess := seedPerfProject(t, s)

	// No auth → 401 on every performance route.
	base := "/api/0/projects/acme-corp/" + slug
	for _, path := range []string{
		base + "/performance/summary/",
		base + "/performance/transaction/?name=x",
		base + "/traces/11111111-1111-4111-8111-111111111111/",
		base + "/releases-health/",
	} {
		if rec := s.get(t, path, nil, nil); rec.Code != 401 {
			t.Fatalf("GET %s unauthenticated = %d, want 401", path, rec.Code)
		}
	}

	// A project id that doesn't exist → 404 even when authed.
	if rec := s.get(t, "/api/0/projects/acme-corp/"+projectID+"-missing/performance/summary/", sess, nil); rec.Code != 404 {
		t.Fatalf("unknown project = %d, want 404", rec.Code)
	}

	// Malformed trace id → 400.
	if rec := s.get(t, "/api/0/projects/acme-corp/"+slug+"/traces/not-a-uuid/", sess, nil); rec.Code != 400 {
		t.Fatalf("bad trace id = %d, want 400", rec.Code)
	}
}
