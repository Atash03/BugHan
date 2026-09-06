package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/Atash03/BugHan/internal/hll"
	"github.com/Atash03/BugHan/internal/testdb"
)

// Crash-free user rates need distinct dids per rollup row (DESIGN.md §9):
// individual session items merge their did into the row's HLL sketches —
// all dids in distinct_did, crashed sessions also in crashed_did. Repeated
// deliveries of the same did must not inflate the estimate.

func sessionItemEnvelope(sid, did, status string, errors int) *Envelope {
	payload, _ := json.Marshal(map[string]any{
		"sid":       sid,
		"did":       did,
		"status":    status,
		"errors":    errors,
		"started":   "2026-09-06T10:30:00Z",
		"timestamp": "2026-09-06T10:35:00Z",
		"duration":  42.5,
		"attrs":     map[string]string{"release": "web@1.0.0", "environment": "production"},
	})
	return &Envelope{Items: []Item{{Header: ItemHeader{Type: "session"}, Payload: payload}}}
}

func TestSessionItemsFeedDistinctDidSketches(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	projectID := seedProject(t, ctx, pool)
	p := NewProcessor(pool)

	for i := 0; i < 30; i++ {
		env := sessionItemEnvelope(
			sprintf("11111111-1111-4111-8111-%012d", i),
			fmt.Sprintf("user-%d", i), "ok", 0)
		if _, err := p.Process(ctx, env, projectID); err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
	}
	// One crashed session reusing an existing did: crashed_did must see it,
	// distinct_did must not grow.
	crashEnv := sessionItemEnvelope(
		"22222222-2222-4222-8222-222222222222", "user-7", "crashed", 1)
	if _, err := p.Process(ctx, crashEnv, projectID); err != nil {
		t.Fatalf("crashed session: %v", err)
	}

	var distinctBlob, crashedBlob []byte
	if err := pool.QueryRow(ctx,
		`SELECT distinct_did, crashed_did FROM session_rollups
		 WHERE project_id = $1 AND release = 'web@1.0.0' AND environment = 'production'`,
		projectID).Scan(&distinctBlob, &crashedBlob); err != nil {
		t.Fatalf("rollup row: %v", err)
	}
	distinct, err := hll.UnmarshalBinary(distinctBlob)
	if err != nil {
		t.Fatalf("distinct_did blob: %v", err)
	}
	crashed, err := hll.UnmarshalBinary(crashedBlob)
	if err != nil {
		t.Fatalf("crashed_did blob: %v", err)
	}
	if d := distinct.Estimate(); d < 27 || d > 33 {
		t.Fatalf("distinct_did estimate = %d, want ≈30", d)
	}
	if c := crashed.Estimate(); c != 1 {
		t.Fatalf("crashed_did estimate = %d, want 1", c)
	}

	// Re-delivery of the same did under a new sid must not grow the sketch.
	again := sessionItemEnvelope(
		"33333333-3333-4333-8333-333333333333", "user-7", "ok", 0)
	if _, err := p.Process(ctx, again, projectID); err != nil {
		t.Fatalf("re-delivery: %v", err)
	}
	var blob2 []byte
	if err := pool.QueryRow(ctx,
		`SELECT distinct_did FROM session_rollups
		 WHERE project_id = $1 AND release = 'web@1.0.0' AND environment = 'production'`,
		projectID).Scan(&blob2); err != nil {
		t.Fatal(err)
	}
	sk2, err := hll.UnmarshalBinary(blob2)
	if err != nil {
		t.Fatal(err)
	}
	if d := sk2.Estimate(); d < 27 || d > 33 {
		t.Fatalf("distinct_did after re-delivery = %d, want still ≈30", d)
	}
}

// Aggregate `sessions` items carry no dids: sketches must survive their
// additive upsert.
func TestSessionAggregatesLeaveSketchesIntact(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	projectID := seedProject(t, ctx, pool)
	p := NewProcessor(pool)

	if _, err := p.Process(ctx, sessionItemEnvelope(
		"44444444-4444-4444-8444-444444444444", "user-1", "ok", 0), projectID); err != nil {
		t.Fatalf("session: %v", err)
	}
	agg := &Envelope{Items: []Item{{
		Header: ItemHeader{Type: "sessions"},
		Payload: []byte(`{
			"attrs": {"release": "web@1.0.0", "environment": "production"},
			"aggregates": [{"started": "2026-09-06T10:05:00Z", "exited": 5}]
		}`),
	}}}
	if _, err := p.Process(ctx, agg, projectID); err != nil {
		t.Fatalf("aggregates: %v", err)
	}
	var blob []byte
	if err := pool.QueryRow(ctx,
		`SELECT distinct_did FROM session_rollups
		 WHERE project_id = $1 AND release = 'web@1.0.0' AND environment = 'production'`,
		projectID).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	sk, err := hll.UnmarshalBinary(blob)
	if err != nil {
		t.Fatal(err)
	}
	if d := sk.Estimate(); d != 1 {
		t.Fatalf("distinct_did after aggregates = %d, want 1 (aggregates must not clobber)", d)
	}
}
