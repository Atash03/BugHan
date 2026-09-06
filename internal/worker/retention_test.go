package worker

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Atash03/BugHan/internal/db"
	"github.com/Atash03/BugHan/internal/testdb"
)

// The retention sweep drops old monthly partitions, DELETEs aged small-table
// rows, keeps rollups within their own window, and never deletes issues —
// they survive raw-event deletion (DESIGN.md §7, ticket #32 §3).
func TestRetentionSweep(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	projectID := seedWorkerProject(t, ctx, pool)
	var issueID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO issues (id, project_id, fingerprint, first_seen, last_seen)
		 VALUES (gen_random_uuid(), $1, 'fp-sweep', now(), now()) RETURNING id`, projectID).Scan(&issueID); err != nil {
		t.Fatal(err)
	}

	// A partition from ~4 months ago (upper bound older than the 90-day event
	// cutoff) plus old and recent telemetry.
	oldMonth := time.Now().UTC().AddDate(0, -4, 0)
	if err := db.EnsurePartitions(ctx, pool, []time.Time{oldMonth}); err != nil {
		t.Fatal(err)
	}
	oldTS := time.Date(oldMonth.Year(), oldMonth.Month(), 10, 12, 0, 0, 0, time.UTC)

	mustExec(t, pool, `INSERT INTO events_part
		(id, project_id, issue_id, timestamp, platform, payload)
		VALUES ($1, $2, $3, $4, 'javascript', '{}')`,
		uuid.NewString(), projectID, issueID, oldTS)
	mustExec(t, pool, `INSERT INTO events_part
		(id, project_id, issue_id, timestamp, platform, payload)
		VALUES ($1, $2, $3, now(), 'javascript', '{}')`,
		uuid.NewString(), projectID, issueID)
	mustExec(t, pool, `INSERT INTO transactions_part
		(id, project_id, timestamp, start_ts, payload)
		VALUES ($1, $2, $3, $3, '{}')`, uuid.NewString(), projectID, oldTS)
	mustExec(t, pool, `INSERT INTO sessions_part
		(sid, started, project_id) VALUES ($1, $2, $3)`, uuid.NewString(), oldTS, projectID)
	mustExec(t, pool, `INSERT INTO feedbacks (id, project_id, message, received_at)
		VALUES ($1, $2, 'old', $3)`, uuid.NewString(), projectID, time.Now().AddDate(0, 0, -100))
	mustExec(t, pool, `INSERT INTO feedbacks (id, project_id, message, received_at)
		VALUES ($1, $2, 'fresh', now())`, uuid.NewString(), projectID)
	mustExec(t, pool, `INSERT INTO issue_user_hashes (issue_id, user_hash, first_seen)
		VALUES ($1, 'old-user', $2)`, issueID, time.Now().AddDate(0, 0, -100))
	mustExec(t, pool, `INSERT INTO issue_user_hashes (issue_id, user_hash, first_seen)
		VALUES ($1, 'fresh-user', now())`, issueID)
	mustExec(t, pool, `INSERT INTO session_rollups (project_id, release, environment, hour, total)
		VALUES ($1, 'r', 'production', $2, 1)`, projectID, oldTS.Truncate(time.Hour))
	mustExec(t, pool, `INSERT INTO session_rollups (project_id, release, environment, hour, total)
		VALUES ($1, 'r', 'production', date_trunc('hour', now()), 1)`, projectID)

	rc := RetentionConfig{
		EventDays:         90,
		TransactionDays:   30,
		SessionDays:       30,
		SessionRollupDays: 90,
		FeedbackDays:      90,
	}
	if err := RunRetentionSweep(ctx, pool, rc); err != nil {
		t.Fatalf("RunRetentionSweep: %v", err)
	}

	// Old raw rows dropped (partitions), recent event survives.
	mustHave(t, pool, 0, `SELECT count(*) FROM transactions_part`)
	mustHave(t, pool, 0, `SELECT count(*) FROM sessions_part`)
	mustHave(t, pool, 1, `SELECT count(*) FROM events_part`)
	mustHave(t, pool, 1, `SELECT count(*) FROM feedbacks WHERE message = 'fresh'`)
	mustHave(t, pool, 0, `SELECT count(*) FROM feedbacks WHERE message = 'old'`)
	mustHave(t, pool, 0, `SELECT count(*) FROM issue_user_hashes WHERE user_hash = 'old-user'`)
	mustHave(t, pool, 1, `SELECT count(*) FROM issue_user_hashes WHERE user_hash = 'fresh-user'`)
	mustHave(t, pool, 1, `SELECT count(*) FROM session_rollups WHERE hour = date_trunc('hour', now())`)
	mustHave(t, pool, 0, `SELECT count(*) FROM session_rollups WHERE hour = $1`, oldTS.Truncate(time.Hour))

	// Issues survive raw-event deletion.
	mustHave(t, pool, 1, `SELECT count(*) FROM issues WHERE id = $1`, issueID)

	// The old month's partitions are gone.
	mustHave(t, pool, 0, `SELECT count(*) FROM pg_class WHERE relname = $1`,
		"events_part_"+oldMonth.Format("2006_01"))
}

func seedWorkerProject(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
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

func mustExec(t *testing.T, pool *pgxpool.Pool, query string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func mustHave(t *testing.T, pool *pgxpool.Pool, want int64, query string, args ...any) {
	t.Helper()
	var got int64
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&got); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	if got != want {
		t.Fatalf("%s: got %d, want %d", query, got, want)
	}
}
