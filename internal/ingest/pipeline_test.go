package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Atash03/BugHan/internal/testdb"
)

func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

// eventIDFor produces distinct, deterministic event ids.
func eventIDFor(i int) string {
	u := uuid.New()
	u[15] = byte(i)
	return u.String()
}

// seedProject provisions a real org+project row (FK target) and returns the
// project id.
func seedProject(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
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

// eventEnvelope builds a minimal envelope carrying one error event item.
func eventEnvelope(eventJSON string) *Envelope {
	var header struct {
		EventID string `json:"event_id"`
	}
	_ = json.Unmarshal([]byte(eventJSON), &header)
	return &Envelope{
		Header: Header{EventID: header.EventID},
		Items: []Item{{
			Header:  ItemHeader{Type: "event"},
			Payload: []byte(eventJSON),
		}},
	}
}

// TestEventCreatesIssueAndLinksEvent: a stored error event must be grouped
// into an issue carrying the normalized title/culprit, with counters and
// hourly rollups (DESIGN.md §6, §8).
func TestEventCreatesIssueAndLinksEvent(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	projectID := seedProject(t, ctx, pool)

	env := eventEnvelope(`{
		"event_id": "c1a2b3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d",
		"timestamp": "2026-09-06T10:30:00Z",
		"platform": "javascript",
		"level": "warning",
		"exception": {"values": [{"type": "TypeError",
			"value": "Order 12345 failed",
			"stacktrace": {"frames": [
				{"filename": "/vendor/lib.js", "function": "dispatch", "in_app": false},
				{"filename": "https://cdn.example.com/assets/main.9ab34f.js?v=2", "function": "onClick", "in_app": true}]}}]}
	}`)

	p := NewProcessor(pool)
	if _, err := p.Process(ctx, env, projectID); err != nil {
		t.Fatalf("Process: %v", err)
	}

	var (
		issueID     string
		title       string
		culprit     string
		count       int64
		status      string
		first, last string
	)
	if err := pool.QueryRow(ctx,
		`SELECT id, title, culprit, count, status, first_seen::text, last_seen::text FROM issues`).
		Scan(&issueID, &title, &culprit, &count, &status, &first, &last); err != nil {
		t.Fatalf("issue row: %v", err)
	}
	if title != "TypeError: Order {num} failed" {
		t.Fatalf("issue title = %q", title)
	}
	if culprit != "https://cdn.example.com/assets/main.{hash}.js in onClick" {
		t.Fatalf("issue culprit = %q", culprit)
	}
	if count != 1 || status != "unresolved" {
		t.Fatalf("issue count=%d status=%s", count, status)
	}

	var eventIssueID *string
	var evTitle string
	if err := pool.QueryRow(ctx,
		`SELECT issue_id::text, title FROM events_part WHERE project_id = $1`, projectID).
		Scan(&eventIssueID, &evTitle); err != nil {
		t.Fatalf("event row: %v", err)
	}
	if eventIssueID == nil || *eventIssueID != issueID {
		t.Fatalf("event not linked to issue: %v", eventIssueID)
	}
	if evTitle != title {
		t.Fatalf("event title %q != issue title %q", evTitle, title)
	}

	var rollupCount int64
	if err := pool.QueryRow(ctx,
		`SELECT count FROM event_rollups WHERE issue_id = $1`, issueID).Scan(&rollupCount); err != nil {
		t.Fatalf("event rollup: %v", err)
	}
	if rollupCount != 1 {
		t.Fatalf("event rollup count = %d", rollupCount)
	}
	var userCount int
	if err := pool.QueryRow(ctx, `SELECT user_count FROM issues WHERE id = $1`, issueID).Scan(&userCount); err != nil {
		t.Fatal(err)
	}
	if userCount != 0 {
		t.Fatalf("user_count = %d, want 0 (event has no user)", userCount)
	}
}

// A second event with the same group key folds into the same issue; distinct
// users count once (issue.user_count), events count individually (issue.count).
func TestEventsAggregateIntoIssue(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	projectID := seedProject(t, ctx, pool)

	payload := `{
		"event_id": "%s",
		"timestamp": "2026-09-06T10:%02d:00Z",
		"exception": {"values": [{"type": "TypeError", "value": "Cannot read x of undefined"}]},
		"user": {"id": %s}
	}`
	p := NewProcessor(pool)
	for i, user := range []string{`"u-1"`, `"u-1"`, `"u-2"`} {
		env := eventEnvelope(sprintf(payload, eventIDFor(i), i, user))
		if _, err := p.Process(ctx, env, projectID); err != nil {
			t.Fatalf("Process %d: %v", i, err)
		}
	}

	var count int64
	var userCount int
	if err := pool.QueryRow(ctx, `SELECT count, user_count FROM issues`).Scan(&count, &userCount); err != nil {
		t.Fatalf("issue row: %v", err)
	}
	if count != 3 {
		t.Fatalf("issue count = %d, want 3", count)
	}
	if userCount != 2 {
		t.Fatalf("issue user_count = %d, want 2 distinct users", userCount)
	}
	var rollups int64
	if err := pool.QueryRow(ctx, `SELECT sum(count) FROM event_rollups`).Scan(&rollups); err != nil {
		t.Fatal(err)
	}
	if rollups != 3 {
		t.Fatalf("rollup total = %d, want 3", rollups)
	}
}

// SDK retries are common: a duplicate event_id must be a 200 no-op — no new
// event row, no counter or rollup bumps (DESIGN.md §6).
func TestDuplicateEventIsNoOp(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	projectID := seedProject(t, ctx, pool)

	payload := `{
		"event_id": "d0d0d0d0-0000-4000-8000-000000000001",
		"timestamp": "2026-09-06T10:30:00Z",
		"exception": {"values": [{"type": "Error", "value": "once"}]}
	}`
	p := NewProcessor(pool)
	env := eventEnvelope(payload)
	res, err := p.Process(ctx, env, projectID)
	if err != nil {
		t.Fatalf("first Process: %v", err)
	}
	if res.Duplicates != 0 {
		t.Fatalf("first delivery marked duplicate: %+v", res)
	}
	res, err = p.Process(ctx, env, projectID)
	if err != nil {
		t.Fatalf("second Process: %v", err)
	}
	if res.Duplicates != 1 {
		t.Fatalf("retry not detected as duplicate: %+v", res)
	}

	var count int64
	var events int64
	if err := pool.QueryRow(ctx, `SELECT count FROM issues`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events_part`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if count != 1 || events != 1 {
		t.Fatalf("duplicate bumped counters: issue count=%d events=%d", count, events)
	}
}

// New event on a resolved issue → unresolved with substatus regressed;
// an ignored issue keeps absorbing events silently (DESIGN.md §8).
func TestIssueLifecycleTransitions(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	projectID := seedProject(t, ctx, pool)

	payload := `{
		"event_id": "%s",
		"timestamp": "2026-09-06T10:%02d:00Z",
		"exception": {"values": [{"type": "Error", "value": "boom %d"}]}
	}`
	p := NewProcessor(pool)
	send := func(i int) {
		env := eventEnvelope(sprintf(payload, eventIDFor(i), i, i))
		if _, err := p.Process(ctx, env, projectID); err != nil {
			t.Fatalf("Process %d: %v", i, err)
		}
	}

	// Event 0 creates the issue; then it's resolved.
	send(0)
	if _, err := pool.Exec(ctx, `UPDATE issues SET status='resolved', substatus=''`); err != nil {
		t.Fatal(err)
	}
	// Event 1 regresses it.
	send(1)
	var status, substatus string
	var count int64
	if err := pool.QueryRow(ctx, `SELECT status, substatus, count FROM issues`).Scan(&status, &substatus, &count); err != nil {
		t.Fatal(err)
	}
	if status != "unresolved" || substatus != "regressed" {
		t.Fatalf("after regression: status=%s substatus=%s", status, substatus)
	}
	if count != 2 {
		t.Fatalf("count after regression = %d, want 2", count)
	}

	// Now ignored: further events are absorbed silently.
	if _, err := pool.Exec(ctx, `UPDATE issues SET status='ignored'`); err != nil {
		t.Fatal(err)
	}
	send(2)
	if err := pool.QueryRow(ctx, `SELECT status, substatus, count FROM issues`).Scan(&status, &substatus, &count); err != nil {
		t.Fatal(err)
	}
	if status != "ignored" {
		t.Fatalf("ignored issue changed status to %s", status)
	}
	if count != 3 {
		t.Fatalf("ignored issue stopped counting: count=%d, want 3", count)
	}
}
