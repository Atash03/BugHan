package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Processor persists parsed envelope items. The T3 slice stores raw items
// with the fields the pipeline needs; grouping/rollups attach in the next
// slice.
type Processor struct {
	pool *pgxpool.Pool
}

func NewProcessor(pool *pgxpool.Pool) *Processor { return &Processor{pool: pool} }

// ProcessResult summarizes one envelope.
type ProcessResult struct {
	Accepted   int // items persisted
	Ignored    int // known types we deliberately don't store (client_report, attachment…)
	Unknown    int // unknown item types (counted per spec)
	Duplicates int // event_id dedupe hits
}

// Process persists each supported item inside one transaction.
func (p *Processor) Process(ctx context.Context, env *Envelope, projectID string) (ProcessResult, error) {
	var res ProcessResult
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer tx.Rollback(ctx)

	for _, item := range env.Items {
		switch item.Header.Type {
		case "event":
			dup, err := p.storeEvent(ctx, tx, env, item, projectID)
			if err != nil {
				return res, fmt.Errorf("event item: %w", err)
			}
			if dup {
				res.Duplicates++
			} else {
				res.Accepted++
			}
		case "transaction":
			dup, err := p.storeTransaction(ctx, tx, env, item, projectID)
			if err != nil {
				return res, fmt.Errorf("transaction item: %w", err)
			}
			if dup {
				res.Duplicates++
			} else {
				res.Accepted++
			}
		case "session":
			if err := p.storeSession(ctx, tx, item, projectID); err != nil {
				return res, fmt.Errorf("session item: %w", err)
			}
			res.Accepted++
		case "sessions":
			if err := p.storeSessionAggregates(ctx, tx, item, projectID); err != nil {
				return res, fmt.Errorf("sessions item: %w", err)
			}
			res.Accepted++
		case "feedback", "user_report":
			if err := p.storeFeedback(ctx, tx, env, item, projectID); err != nil {
				return res, fmt.Errorf("%s item: %w", item.Header.Type, err)
			}
			res.Accepted++
		case "client_report", "attachment", "profile", "profile_chunk", "replay_event",
			"replay_recording", "replay_video", "check_in", "log", "otel_log", "span",
			"trace_metric", "security":
			res.Ignored++ // known but out of scope — tolerated per spec
		default:
			res.Unknown++
			if _, err := tx.Exec(ctx, `
				INSERT INTO unknown_item_stats (project_id, item_type, count)
				VALUES ($1, $2, 1)
				ON CONFLICT (project_id, item_type, day) DO UPDATE SET count = unknown_item_stats.count + 1`,
				projectID, item.Header.Type); err != nil {
				return res, err
			}
		}
	}
	return res, tx.Commit(ctx)
}

// ---- shared payload field extraction ----

type eventPayload struct {
	EventID     string          `json:"event_id"`
	Timestamp   json.RawMessage `json:"timestamp"`
	Platform    string          `json:"platform"`
	Level       string          `json:"level"`
	Environment string          `json:"environment"`
	Release     string          `json:"release"`
	Dist        string          `json:"dist"`
	Message     string          `json:"message"`
	Tags        json.RawMessage `json:"tags"`
	User        struct {
		ID       string `json:"id"`
		Email    string `json:"email"`
		Username string `json:"username"`
		IP       string `json:"ip_address"`
	} `json:"user"`
	Contexts struct {
		Trace struct {
			TraceID      string `json:"trace_id"`
			SpanID       string `json:"span_id"`
			ParentSpanID string `json:"parent_span_id"`
			Op           string `json:"op"`
			Status       string `json:"status"`
		} `json:"trace"`
	} `json:"contexts"`
	Transaction string          `json:"transaction"`
	Fingerprint []string        `json:"fingerprint"`
	Extra       json.RawMessage `json:"extra"`
	SDK         json.RawMessage `json:"sdk"`
}

// parseTimestamp accepts RFC 3339 strings or unix seconds (int/float).
func parseTimestamp(raw json.RawMessage) (time.Time, error) {
	if len(raw) == 0 {
		return time.Time{}, fmt.Errorf("missing timestamp")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05"} {
			if t, err := time.Parse(layout, s); err == nil {
				return t, nil
			}
		}
		return time.Time{}, fmt.Errorf("unparseable timestamp %q", s)
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return time.UnixMilli(int64(f * 1000)).UTC(), nil
	}
	return time.Time{}, fmt.Errorf("bad timestamp %s", raw)
}

// eventIDFrom resolves the item's event id: the envelope header wins over the
// payload (per spec).
func eventIDFrom(env *Envelope, payload eventPayload) (uuid.UUID, error) {
	idStr := env.Header.EventID
	if idStr == "" {
		idStr = payload.EventID
	}
	if idStr == "" {
		return uuid.Nil, fmt.Errorf("missing event_id")
	}
	id, err := uuid.Parse(strings.ToLower(idStr))
	if err != nil {
		return uuid.Nil, fmt.Errorf("bad event_id %q", idStr)
	}
	return id, nil
}

func userHash(u eventPayloadUser) string {
	if u.ID == "" && u.Email == "" && u.Username == "" && u.IP == "" {
		return ""
	}
	ident := u.ID
	if ident == "" {
		ident = u.Email
	}
	if ident == "" {
		ident = u.Username
	}
	if ident == "" {
		ident = u.IP
	}
	sum := sha256.Sum256([]byte(ident))
	return hex.EncodeToString(sum[:16])
}

type eventPayloadUser = struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	Username string `json:"username"`
	IP       string `json:"ip_address"`
}

func parseTags(raw json.RawMessage) map[string]string {
	if len(raw) == 0 {
		return map[string]string{}
	}
	out := map[string]string{}
	// Canonical shape: map; some SDKs send an array of {key,value} pairs —
	// tolerate both per the "legacy/non-canonical shapes" rule.
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err == nil {
		return m
	}
	var arr []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &arr); err == nil {
		for _, kv := range arr {
			out[kv.Key] = kv.Value
		}
	}
	return out
}

// touchRelease upserts the implicit release row for events/sessions that
// declare one.
func touchRelease(ctx context.Context, q pgx.Tx, projectID, release string, ts time.Time) error {
	if release == "" {
		return nil
	}
	_, err := q.Exec(ctx, `
		INSERT INTO releases (id, project_id, version, first_event_at, last_event_at)
		VALUES ($1, $2, $3, $4, $4)
		ON CONFLICT (project_id, version) DO UPDATE
		SET last_event_at = GREATEST(releases.last_event_at, EXCLUDED.last_event_at),
		    first_event_at = COALESCE(releases.first_event_at, EXCLUDED.first_event_at)`,
		uuid.NewString(), projectID, release, ts)
	return err
}
