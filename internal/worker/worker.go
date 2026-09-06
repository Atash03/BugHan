// Package worker drains the jobs table in-process (DESIGN.md §2): a bounded
// polling loop claiming due jobs with FOR UPDATE SKIP LOCKED, so multiple
// worker processes would also be safe. Retries use exponential backoff; jobs
// exceeding max_attempts are marked failed.
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Handler executes one job.
type Handler func(ctx context.Context, payload json.RawMessage) error

// Worker is the embedded job runner.
type Worker struct {
	pool     *pgxpool.Pool
	log      *slog.Logger
	handlers map[string]Handler
	interval time.Duration // poll cadence; shortened in tests
}

func New(pool *pgxpool.Pool, log *slog.Logger) *Worker {
	return &Worker{
		pool:     pool,
		log:      log,
		handlers: map[string]Handler{},
		interval: 5 * time.Second,
	}
}

// Register wires a job kind to its handler.
func (w *Worker) Register(kind string, h Handler) { w.handlers[kind] = h }

// Enqueue schedules a job. A nil payload is stored as {}.
func (w *Worker) Enqueue(ctx context.Context, kind string, payload json.RawMessage, runAt time.Time) error {
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	_, err := w.pool.Exec(ctx,
		`INSERT INTO jobs (kind, payload, run_at) VALUES ($1, $2, $3)`,
		kind, payload, runAt)
	return err
}

// Start runs the polling loop until ctx is cancelled.
func (w *Worker) Start(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for {
				ran, err := w.runDue(ctx)
				if err != nil {
					w.log.Error("worker", "err", err)
					break
				}
				if ran == 0 {
					break
				}
			}
		}
	}
}

// runDue claims and executes every due job; returns how many ran.
func (w *Worker) runDue(ctx context.Context) (int, error) {
	ran := 0
	for {
		claimed, err := w.claim(ctx)
		if err != nil {
			return ran, err
		}
		if claimed == nil {
			return ran, nil
		}
		w.execute(ctx, claimed)
		ran++
	}
}

type claimedJob struct {
	id          int64
	kind        string
	payload     json.RawMessage
	attempts    int
	maxAttempts int
}

// claim atomically moves one due pending job to running.
func (w *Worker) claim(ctx context.Context) (*claimedJob, error) {
	var j claimedJob
	err := w.pool.QueryRow(ctx, `
		UPDATE jobs SET status = 'running', attempts = attempts + 1, updated_at = now()
		WHERE id = (
			SELECT id FROM jobs
			WHERE status = 'pending' AND run_at <= now()
			ORDER BY run_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING id, kind, payload, attempts, max_attempts`).
		Scan(&j.id, &j.kind, &j.payload, &j.attempts, &j.maxAttempts)
	if err != nil {
		return nil, nil //nolint: nilerr — pgx returns ErrNoRows when nothing is due
	}
	return &j, nil
}

// execute runs the handler and records the outcome (done, retry with backoff,
// or failed after max attempts).
func (w *Worker) execute(ctx context.Context, j *claimedJob) {
	h, ok := w.handlers[j.kind]
	var err error
	if !ok {
		err = fmt.Errorf("no handler registered for kind %q", j.kind)
	} else {
		err = h(ctx, j.payload)
	}

	switch {
	case err == nil:
		_, e := w.pool.Exec(ctx,
			`UPDATE jobs SET status = 'done', updated_at = now() WHERE id = $1`, j.id)
		if e != nil {
			w.log.Error("mark done", "job_id", j.id, "err", e)
		}
	case j.attempts >= j.maxAttempts:
		w.log.Error("job failed permanently", "kind", j.kind, "job_id", j.id, "err", err)
		_, e := w.pool.Exec(ctx,
			`UPDATE jobs SET status = 'failed', last_error = $2, updated_at = now() WHERE id = $1`,
			j.id, err.Error())
		if e != nil {
			w.log.Error("mark failed", "job_id", j.id, "err", e)
		}
	default:
		backoff := time.Duration(1<<uint(j.attempts-1)) * time.Minute
		w.log.Warn("job failed, retrying", "kind", j.kind, "job_id", j.id,
			"attempt", j.attempts, "backoff", backoff, "err", err)
		if _, e := w.pool.Exec(ctx,
			`UPDATE jobs SET status = 'pending', run_at = $2, last_error = $3, updated_at = now() WHERE id = $1`,
			j.id, time.Now().UTC().Add(backoff), err.Error()); e != nil {
			w.log.Error("mark retry", "job_id", j.id, "err", e)
		}
	}
}
