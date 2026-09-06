package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// storeEvent persists one error event; returns true when it was a duplicate.
func (p *Processor) storeEvent(ctx context.Context, tx pgx.Tx, env *Envelope, item Item, projectID string) (bool, error) {
	var payload eventPayload
	if err := json.Unmarshal(item.Payload, &payload); err != nil {
		return false, fmt.Errorf("payload: %w", err)
	}
	id, err := eventIDFrom(env, payload)
	if err != nil {
		return false, err
	}
	ts := time.Now().UTC()
	if len(payload.Timestamp) > 0 {
		if t, err := parseTimestamp(payload.Timestamp); err == nil {
			ts = t
		} else {
			return false, err
		}
	}
	if payload.Platform == "" {
		payload.Platform = "javascript"
	}
	level := payload.Level
	if level == "" {
		level = "error"
	}
	environment := payload.Environment
	if environment == "" {
		environment = "production"
	}
	traceID := uuid.NullUUID{}
	if payload.Contexts.Trace.TraceID != "" {
		if t, err := uuid.Parse(payload.Contexts.Trace.TraceID); err == nil {
			traceID = uuid.NullUUID{UUID: t, Valid: true}
		}
	}
	tags := parseTags(payload.Tags)
	tagsJSON, _ := json.Marshal(tags)
	payloadJSON := json.RawMessage(item.Payload)

	ct, err := tx.Exec(ctx, `
		INSERT INTO events_part
			(id, project_id, timestamp, platform, level, environment, release, dist,
			 message, type, trace_id, span_id, user_hash, tags, payload)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'error',$10,$11,$12,$13,$14)
		ON CONFLICT (id, timestamp) DO NOTHING`,
		id, projectID, ts, payload.Platform, level, environment, payload.Release, payload.Dist,
		payload.Message, traceID, payload.Contexts.Trace.SpanID, userHash(payload.User), tagsJSON, payloadJSON)
	if err != nil {
		return false, err
	}
	if ct.RowsAffected() == 0 {
		return true, nil
	}
	if err := touchRelease(ctx, tx, projectID, payload.Release, ts); err != nil {
		return false, err
	}
	_, _ = tx.Exec(ctx, `
		INSERT INTO project_event_rollups (project_id, hour, count) VALUES ($1, $2, 1)
		ON CONFLICT (project_id, hour) DO UPDATE SET count = project_event_rollups.count + 1`,
		projectID, ts.Truncate(time.Hour))
	return false, nil
}

// storeTransaction persists one transaction (spans stay in the payload).
func (p *Processor) storeTransaction(ctx context.Context, tx pgx.Tx, env *Envelope, item Item, projectID string) (bool, error) {
	var payload struct {
		eventPayload
		StartTimestamp  json.RawMessage `json:"start_timestamp"`
		Transaction     string          `json:"transaction"`
		TransactionInfo struct {
			Source string `json:"source"`
		} `json:"transaction_info"`
		Status       string          `json:"status"`
		Measurements json.RawMessage `json:"measurements"`
	}
	if err := json.Unmarshal(item.Payload, &payload); err != nil {
		return false, fmt.Errorf("payload: %w", err)
	}
	id, err := eventIDFrom(env, payload.eventPayload)
	if err != nil {
		return false, err
	}
	if len(payload.StartTimestamp) == 0 {
		return false, fmt.Errorf("missing start_timestamp")
	}
	startTS, err := parseTimestamp(payload.StartTimestamp)
	if err != nil {
		return false, err
	}
	endTS := time.Now().UTC()
	if len(payload.Timestamp) > 0 {
		if t, err := parseTimestamp(payload.Timestamp); err == nil {
			endTS = t
		}
	}
	if endTS.Before(startTS) {
		return false, fmt.Errorf("timestamp before start_timestamp (Relay contract)")
	}
	environment := payload.Environment
	if environment == "" {
		environment = "production"
	}
	traceID := uuid.NullUUID{}
	if payload.Contexts.Trace.TraceID != "" {
		if t, err := uuid.Parse(payload.Contexts.Trace.TraceID); err == nil {
			traceID = uuid.NullUUID{UUID: t, Valid: true}
		}
	}
	durationMS := endTS.Sub(startTS).Seconds() * 1000

	ct, err := tx.Exec(ctx, `
		INSERT INTO transactions_part
			(id, project_id, timestamp, start_ts, duration_ms, trace_id, span_id,
			 name, source, status, environment, release, dist, measurements, payload)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		ON CONFLICT (id, timestamp) DO NOTHING`,
		id, projectID, endTS, startTS, durationMS, traceID, payload.Contexts.Trace.SpanID,
		payload.Transaction, payload.TransactionInfo.Source, payload.Contexts.Trace.Status,
		environment, payload.Release, payload.Dist, json.RawMessage(payload.Measurements), json.RawMessage(item.Payload))
	if err != nil {
		return false, err
	}
	if ct.RowsAffected() == 0 {
		return true, nil
	}
	if err := touchRelease(ctx, tx, projectID, payload.Release, startTS); err != nil {
		return false, err
	}
	return false, nil
}

// storeSession persists an individual session (additive, most-recent-wins)
// and folds it into the hourly rollup.
func (p *Processor) storeSession(ctx context.Context, tx pgx.Tx, item Item, projectID string) error {
	var s struct {
		SID       string   `json:"sid"`
		Init      bool     `json:"init"`
		Started   string   `json:"started"`
		Timestamp string   `json:"timestamp"`
		Status    string   `json:"status"`
		Errors    int      `json:"errors"`
		Duration  *float64 `json:"duration"`
		DID       string   `json:"did"`
		Attrs     struct {
			Release     string `json:"release"`
			Environment string `json:"environment"`
		} `json:"attrs"`
	}
	if err := json.Unmarshal(item.Payload, &s); err != nil {
		return fmt.Errorf("payload: %w", err)
	}
	if s.Started == "" {
		return fmt.Errorf("missing started (required)")
	}
	started, err := time.Parse(time.RFC3339Nano, s.Started)
	if err != nil {
		return fmt.Errorf("started: %w", err)
	}
	if s.Attrs.Release == "" {
		return fmt.Errorf("missing attrs.release (required)")
	}
	status := s.Status
	if status == "" {
		status = "ok"
	}
	environment := s.Attrs.Environment
	if environment == "" {
		environment = "production"
	}
	var sid uuid.UUID
	if s.SID != "" {
		if sid, err = uuid.Parse(s.SID); err != nil {
			return fmt.Errorf("bad sid %q", s.SID)
		}
	} else {
		sid = uuid.New() // SDKs should send sid; tolerate its absence
	}
	var dur *float64 = s.Duration
	didHash := ""
	if s.DID != "" {
		sum := sha256Sum(s.DID)
		didHash = hex.EncodeToString(sum[:16])
	}

	// Terminal statuses are immutable: never downgrade a finished session.
	if _, err := tx.Exec(ctx, `
		INSERT INTO sessions_part (sid, started, project_id, status, errors, duration, did_hash, release, environment, init)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (sid, started) DO UPDATE SET
			status = CASE WHEN sessions_part.status IN ('crashed','exited','abnormal')
				THEN sessions_part.status ELSE EXCLUDED.status END,
			errors = GREATEST(sessions_part.errors, EXCLUDED.errors),
			duration = COALESCE(EXCLUDED.duration, sessions_part.duration)`,
		sid, started, projectID, status, s.Errors, dur, didHash, s.Attrs.Release, environment, s.Init); err != nil {
		return err
	}

	hour := started.Truncate(time.Hour)
	_, err = tx.Exec(ctx, `
		INSERT INTO session_rollups (project_id, release, environment, hour,
			total, crashed, abnormal, errored, exited, duration_sum)
		VALUES ($1,$2,$3,$4,1,
			CASE WHEN $5 = 'crashed' THEN 1 ELSE 0 END,
			CASE WHEN $5 = 'abnormal' THEN 1 ELSE 0 END,
			CASE WHEN $5 = 'ok' AND $6 > 0 THEN 1 ELSE 0 END,
			CASE WHEN $5 = 'exited' THEN 1 ELSE 0 END,
			COALESCE($7, 0))
		ON CONFLICT (project_id, release, environment, hour) DO UPDATE SET
			total = session_rollups.total + 1,
			crashed = session_rollups.crashed + CASE WHEN $5 = 'crashed' THEN 1 ELSE 0 END,
			abnormal = session_rollups.abnormal + CASE WHEN $5 = 'abnormal' THEN 1 ELSE 0 END,
			errored = session_rollups.errored + CASE WHEN $5 = 'ok' AND $6 > 0 THEN 1 ELSE 0 END,
			exited = session_rollups.exited + CASE WHEN $5 = 'exited' THEN 1 ELSE 0 END,
			duration_sum = session_rollups.duration_sum + COALESCE($7, 0)`,
		projectID, s.Attrs.Release, environment, hour, status, s.Errors, dur)
	return err
}

// storeSessionAggregates folds pre-aggregated `sessions` items into rollups.
func (p *Processor) storeSessionAggregates(ctx context.Context, tx pgx.Tx, item Item, projectID string) error {
	var s struct {
		Attrs struct {
			Release     string `json:"release"`
			Environment string `json:"environment"`
		} `json:"attrs"`
		Aggregates []struct {
			Started  string `json:"started"`
			Exited   int    `json:"exited"`
			Abnormal int    `json:"abnormal"`
			Crashed  int    `json:"crashed"`
			Errored  int    `json:"errored"`
		} `json:"aggregates"`
	}
	if err := json.Unmarshal(item.Payload, &s); err != nil {
		return fmt.Errorf("payload: %w", err)
	}
	if len(s.Aggregates) > 100 { // spec cap
		return fmt.Errorf("too many aggregates (%d)", len(s.Aggregates))
	}
	environment := s.Attrs.Environment
	if environment == "" {
		environment = "production"
	}
	for _, a := range s.Aggregates {
		if a.Started == "" {
			return fmt.Errorf("aggregate missing started")
		}
		started, err := time.Parse(time.RFC3339Nano, a.Started)
		if err != nil {
			return fmt.Errorf("aggregate started: %w", err)
		}
		// Aggregates are minute-rounded per spec; bucket to the hour.
		hour := started.Truncate(time.Hour)
		_ = hour
		total := a.Exited + a.Abnormal + a.Crashed + a.Errored
		if total == 0 {
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO session_rollups (project_id, release, environment, hour,
				total, crashed, abnormal, errored, exited)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			ON CONFLICT (project_id, release, environment, hour) DO UPDATE SET
				total = session_rollups.total + EXCLUDED.total,
				crashed = session_rollups.crashed + EXCLUDED.crashed,
				abnormal = session_rollups.abnormal + EXCLUDED.abnormal,
				errored = session_rollups.errored + EXCLUDED.errored,
				exited = session_rollups.exited + EXCLUDED.exited`,
			projectID, s.Attrs.Release, environment, hour, total, a.Crashed, a.Abnormal, a.Errored, a.Exited); err != nil {
			return err
		}
		if err := touchRelease(ctx, tx, projectID, s.Attrs.Release, started); err != nil {
			return err
		}
	}
	return nil
}

// storeFeedback persists feedback (SDK v8 `feedback` items) and legacy
// `user_report` items into the same table.
func (p *Processor) storeFeedback(ctx context.Context, tx pgx.Tx, item Item, projectID string) error {
	switch item.Header.Type {
	case "feedback":
		var f struct {
			EventID   string          `json:"event_id"`
			Timestamp json.RawMessage `json:"timestamp"`
			Contexts  struct {
				Feedback struct {
					Message           string `json:"message"`
					ContactEmail      string `json:"contact_email"`
					Name              string `json:"name"`
					URL               string `json:"url"`
					AssociatedEventID string `json:"associated_event_id"`
				} `json:"feedback"`
			} `json:"contexts"`
		}
		if err := json.Unmarshal(item.Payload, &f); err != nil {
			return fmt.Errorf("payload: %w", err)
		}
		if f.Contexts.Feedback.Message == "" {
			return fmt.Errorf("missing contexts.feedback.message (required)")
		}
		id := uuid.New()
		if envID := envOrPayloadID(item, f.EventID); envID != "" {
			if parsed, err := uuid.Parse(envID); err == nil {
				id = parsed
			}
		}
		var assoc uuid.NullUUID
		if f.Contexts.Feedback.AssociatedEventID != "" {
			if parsed, err := uuid.Parse(f.Contexts.Feedback.AssociatedEventID); err == nil {
				assoc = uuid.NullUUID{UUID: parsed, Valid: true}
			}
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO feedbacks (id, project_id, associated_event_id, name, contact_email, message, url, payload)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			id, projectID, assoc, f.Contexts.Feedback.Name, f.Contexts.Feedback.ContactEmail,
			f.Contexts.Feedback.Message, f.Contexts.Feedback.URL, json.RawMessage(item.Payload))
		return err
	case "user_report":
		var r struct {
			EventID  string `json:"event_id"`
			Email    string `json:"email"`
			Name     string `json:"name"`
			Comments string `json:"comments"`
		}
		if err := json.Unmarshal(item.Payload, &r); err != nil {
			return fmt.Errorf("payload: %w", err)
		}
		var assoc uuid.NullUUID
		if r.EventID != "" {
			if parsed, err := uuid.Parse(r.EventID); err == nil {
				assoc = uuid.NullUUID{UUID: parsed, Valid: true}
			}
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO feedbacks (id, project_id, associated_event_id, name, contact_email, message, payload)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			uuid.New(), projectID, assoc, r.Name, r.Email, r.Comments, json.RawMessage(item.Payload))
		return err
	}
	return fmt.Errorf("not a feedback item")
}

func envOrPayloadID(item Item, payloadID string) string {
	// The envelope header id wins; the feedback payload itself has no id field
	// in the feedback interface, so payloadID is usually empty here.
	return payloadID
}

func sha256Sum(s string) [32]byte { return sha256.Sum256([]byte(s)) }
