package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/Atash03/BugHan/internal/config"
	"github.com/Atash03/BugHan/internal/testdb"
	"log/slog"
)

func TestHealthOK(t *testing.T) {
	pool := testdb.New(t)
	cfg := &config.Config{}
	s := NewWithVersion(cfg, pool, slog.Default(), "v0.1.0-test")

	for _, path := range []string{"/api/health/", "/api/health"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)

		if rec.Code != 200 {
			t.Fatalf("%s status = %d, want 200", path, rec.Code)
		}
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s decode body: %v", path, err)
		}
		if body["status"] != "ok" {
			t.Fatalf("%s status field = %q", path, body["status"])
		}
		if body["version"] != "v0.1.0-test" {
			t.Fatalf("%s version field = %q, want stamped binary version", path, body["version"])
		}
		if body["time"] == "" {
			t.Fatalf("%s missing server time field", path)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("%s Cache-Control = %q, want no-store", path, cc)
		}
	}
}

func TestHealthWithEmptyContext(t *testing.T) {
	// Liveness contract: handler must respect request cancellation cleanly.
	pool := testdb.New(t)
	s := New(&config.Config{}, pool, slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("GET", "/api/health/", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	// Either 200 (fast ping) or 503 (cancelled) are acceptable; must not hang or 5xx-panic.
	if rec.Code != 200 && rec.Code != 503 {
		t.Fatalf("status = %d", rec.Code)
	}
}
