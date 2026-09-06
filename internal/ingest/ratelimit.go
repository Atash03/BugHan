package ingest

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RateLimiter is a fixed-window per-project counter backed by
// ingest_counters. limit ≤ 0 disables limiting.
type RateLimiter struct {
	pool  *pgxpool.Pool
	Limit int64
}

func NewRateLimiter(pool *pgxpool.Pool, limitPerMinute int64) *RateLimiter {
	return &RateLimiter{pool: pool, Limit: limitPerMinute}
}

// Check consumes one slot for the project in the current window. It returns
// (allowed, retryAfter, remaining).
func (r *RateLimiter) Check(ctx context.Context, projectID string) (bool, time.Duration, int64) {
	if r.Limit <= 0 {
		return true, 0, 0
	}
	now := time.Now().UTC()
	windowStart := now.Truncate(time.Minute)
	retryAfter := windowStart.Add(time.Minute).Sub(now)

	var count int64
	err := r.pool.QueryRow(ctx, `
		INSERT INTO ingest_counters (project_id, window_start, count)
		VALUES ($1, $2, 1)
		ON CONFLICT (project_id, window_start)
		DO UPDATE SET count = ingest_counters.count + 1
		RETURNING count`, projectID, windowStart).Scan(&count)
	if err != nil {
		// Fail open: a limiter outage must not drop telemetry.
		return true, 0, 0
	}
	remaining := r.Limit - count
	if remaining < 0 {
		remaining = 0
	}
	return count <= r.Limit, retryAfter, remaining
}

// QuotaHeaders builds the SDK-facing rate-limit response headers per the
// Sentry protocol: Retry-After (seconds) and X-Sentry-Rate-Limits
// (retry_after:categories:scope:reason).
func QuotaHeaders(retryAfter time.Duration, remaining int64) [][2]string {
	secs := int64(retryAfter.Seconds() + 0.5)
	if secs < 1 {
		secs = 1
	}
	return [][2]string{
		{"Retry-After", fmt.Sprintf("%d", secs)},
		{"X-Sentry-Rate-Limits", fmt.Sprintf("%d:error;transaction;session;feedback:project:quota_exceeded", secs)},
		{"X-Sentry-Rate-Limit-Remaining", fmt.Sprintf("%d", remaining)},
	}
}
