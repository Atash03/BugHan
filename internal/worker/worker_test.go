package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/Atash03/BugHan/internal/testdb"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&nullWriter{}, nil))
}

type nullWriter struct{}

func (*nullWriter) Write(p []byte) (int, error) { return len(p), nil }

// The embedded worker drains the jobs table: an enqueued job whose run_at is
// due gets claimed and executed, then marked done (DESIGN.md §2, §6).
func TestWorkerRunsDueJob(t *testing.T) {
	pool := testdb.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := New(pool, testLogger())
	w.interval = 5 * time.Millisecond

	ran := make(chan json.RawMessage, 1)
	w.Register("test_kind", func(_ context.Context, payload json.RawMessage) error {
		ran <- payload
		return nil
	})
	go w.Start(ctx)

	if err := w.Enqueue(ctx, "test_kind", json.RawMessage(`{"x":1}`), time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	select {
	case payload := <-ran:
		var got map[string]any
		if err := json.Unmarshal(payload, &got); err != nil {
			t.Fatalf("handler payload %s: %v", payload, err)
		}
		if got["x"] != float64(1) {
			t.Fatalf("handler payload = %s", payload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("handler never ran")
	}

	var status string
	for range 100 {
		err := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE kind = 'test_kind'`).Scan(&status)
		if err == nil && status == "done" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job not marked done, last status = %q", status)
}

// A failing job retries with backoff until max_attempts, then is marked
// failed with the last error (DESIGN.md §12 retry contract).
func TestWorkerRetriesThenFails(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	w := New(pool, testLogger())
	w.interval = time.Hour // no polling; the test drives runDue directly
	attempts := 0
	w.Register("flaky", func(_ context.Context, _ json.RawMessage) error {
		attempts++
		return fmt.Errorf("boom %d", attempts)
	})

	if err := w.Enqueue(ctx, "flaky", nil, time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Drive the loop past each backoff window by resetting run_at.
	var status, lastErr string
	for range 10 {
		if _, err := w.runDue(ctx); err != nil {
			t.Fatalf("runDue: %v", err)
		}
		if err := pool.QueryRow(ctx,
			`SELECT status, last_error FROM jobs WHERE kind = 'flaky'`).Scan(&status, &lastErr); err != nil {
			t.Fatal(err)
		}
		if status == "failed" {
			break
		}
		if _, err := pool.Exec(ctx,
			`UPDATE jobs SET run_at = now() - interval '1 minute' WHERE kind = 'flaky'`); err != nil {
			t.Fatal(err)
		}
	}
	if status != "failed" {
		t.Fatalf("job never failed permanently, status = %q", status)
	}
	if attempts != 3 {
		t.Fatalf("handler attempts = %d, want 3 (max_attempts)", attempts)
	}
	if lastErr != "boom 3" {
		t.Fatalf("last_error = %q, want the final attempt's error", lastErr)
	}
}
