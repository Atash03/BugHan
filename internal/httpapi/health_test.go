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
	s := New(cfg, pool, slog.Default())

	req := httptest.NewRequest("GET", "/api/health/", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("status field = %q", body["status"])
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
