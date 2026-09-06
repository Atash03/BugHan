package ingest

import (
	"context"
	"testing"
	"time"

	"github.com/Atash03/BugHan/internal/testdb"
)

func TestRateLimiterWindow(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	// ingest_counters has an FK to projects — provision a real one.
	projectID := "11111111-1111-1111-1111-111111111111"
	if _, err := pool.Exec(ctx,
		`INSERT INTO organizations (id, name, slug) VALUES ('22222222-2222-2222-2222-222222222222', 'RL Org', 'rl-org')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO projects (id, org_id, name, slug) VALUES ($1, '22222222-2222-2222-2222-222222222222', 'RL', 'rl')`,
		projectID); err != nil {
		t.Fatal(err)
	}
	rl := NewRateLimiter(pool, 2)

	for i := 0; i < 2; i++ {
		allowed, _, _ := rl.Check(ctx, projectID)
		if !allowed {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}
	allowed, retryAfter, _ := rl.Check(ctx, projectID)
	if allowed {
		t.Fatal("third request should be limited")
	}
	if retryAfter <= 0 || retryAfter > time.Minute {
		t.Fatalf("retryAfter = %v", retryAfter)
	}
	hdrs := QuotaHeaders(retryAfter, 0)
	found := map[string]string{}
	for _, kv := range hdrs {
		found[kv[0]] = kv[1]
	}
	if _, ok := found["Retry-After"]; !ok {
		t.Fatal("missing Retry-After header")
	}
	rlHeader, ok := found["X-Sentry-Rate-Limits"]
	if !ok || !containsAll(rlHeader, []string{"error", "transaction", "session", "project"}) {
		t.Fatalf("X-Sentry-Rate-Limits = %q", rlHeader)
	}
}

func TestRateLimiterUnlimited(t *testing.T) {
	pool := testdb.New(t)
	rl := NewRateLimiter(pool, 0) // 0 = disabled (self-host default)
	for i := 0; i < 100; i++ {
		if allowed, _, _ := rl.Check(context.Background(), "p"); !allowed {
			t.Fatalf("request %d limited despite unlimited config", i)
		}
	}
}

func containsAll(s string, subs []string) bool {
	for _, sub := range subs {
		if !contains(s, sub) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
