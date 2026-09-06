package httpapi

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// ingestSetup provisions an org/project/key through the API and returns the
// project id + ingest key (sentry_key).
func ingestSetup(t *testing.T, s *Server) (projectID, sentryKey string) {
	setupAccount(t, s, "neo@acme.dev")
	sess, csrf := login(t, s, "neo@acme.dev", "hunter2hunter2")
	rec := s.apiCall(t, "POST", "/api/0/tokens/", `{"name":"it"}`, sess, map[string]string{"X-CSRF-Token": csrf})
	var tok struct{ Token string }
	_ = json.Unmarshal(rec.Body.Bytes(), &tok)
	bearer := map[string]string{"Authorization": "Bearer " + tok.Token}

	rec = s.apiCall(t, "POST", "/api/0/organizations/", `{"name":"Ingest Org"}`, jar{}, bearer)
	var org struct{ Slug string }
	_ = json.Unmarshal(rec.Body.Bytes(), &org)

	rec = s.apiCall(t, "POST", "/api/0/organizations/"+org.Slug+"/projects/", `{"name":"Web App"}`, jar{}, bearer)
	var proj struct {
		ID   string `json:"id"`
		Keys []struct {
			DSN string `json:"dsn"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == "" {
		t.Fatalf("project create = %d %s", rec.Code, rec.Body.String())
	}
	// dsn = https://<key>@<host>/<projectID>
	dsn := proj.Keys[0].DSN
	at := strings.Index(dsn, "https://") + len("https://")
	key := dsn[at:strings.Index(dsn, "@")]
	return proj.ID, key
}

func (s *Server) postEnvelope(t *testing.T, projectID, key, body, contentEncoding string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/"+projectID+"/envelope/?sentry_version=7&sentry_key="+key+"&sentry_client=sentry.javascript.browser/10.70.0", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-sentry-envelope")
	if contentEncoding != "" {
		req.Header.Set("Content-Encoding", contentEncoding)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestEnvelopeIngestEndToEnd(t *testing.T) {
	s := newTestServer(t)
	projectID, key := ingestSetup(t, s)

	// Error event → 200 and stored.
	rec := s.postEnvelope(t, projectID, key, errorEnvelopeTest, "")
	if rec.Code != 200 {
		t.Fatalf("error envelope = %d: %s", rec.Code, rec.Body.String())
	}
	var row struct{ Count int }
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM events_part WHERE project_id = $1`, projectID).Scan(&row.Count); err != nil || row.Count != 1 {
		t.Fatalf("events stored = %d err=%v", row.Count, err)
	}

	// Duplicate event_id → still 200, no second row.
	rec = s.postEnvelope(t, projectID, key, errorEnvelopeTest, "")
	if rec.Code != 200 {
		t.Fatalf("duplicate envelope = %d", rec.Code)
	}
	_ = s.pool.QueryRow(t.Context(), `SELECT count(*) FROM events_part WHERE project_id = $1`, projectID).Scan(&row.Count)
	if row.Count != 1 {
		t.Fatalf("dedupe failed: %d rows", row.Count)
	}

	// Transaction → 200, stored with trace + duration.
	rec = s.postEnvelope(t, projectID, key, transactionEnvelopeTest, "")
	if rec.Code != 200 {
		t.Fatalf("transaction envelope = %d: %s", rec.Code, rec.Body.String())
	}
	var txRow struct {
		Name         string
		DurationMS   float64
		Measurements map[string]any
	}
	if err := s.pool.QueryRow(t.Context(), `SELECT name, duration_ms, measurements FROM transactions_part WHERE project_id = $1`, projectID).
		Scan(&txRow.Name, &txRow.DurationMS, &txRow.Measurements); err != nil {
		t.Fatalf("transaction stored: %v", err)
	}
	if txRow.Name != "/users/:id" || txRow.DurationMS < 1200 || txRow.DurationMS > 1400 {
		t.Fatalf("transaction = %+v", txRow)
	}
	if _, ok := txRow.Measurements["lcp"]; !ok {
		t.Fatalf("lcp measurement lost: %v", txRow.Measurements)
	}

	// Session → 200, session row + rollup (crashed).
	rec = s.postEnvelope(t, projectID, key, sessionEnvelopeTest, "")
	if rec.Code != 200 {
		t.Fatalf("session envelope = %d: %s", rec.Code, rec.Body.String())
	}
	var r struct{ Total, Crashed int64 }
	if err := s.pool.QueryRow(t.Context(),
		`SELECT total, crashed FROM session_rollups WHERE project_id = $1`, projectID).Scan(&r.Total, &r.Crashed); err != nil || r.Total != 1 || r.Crashed != 1 {
		t.Fatalf("session rollup = %+v err=%v", r, err)
	}

	// Session aggregates → 200, folded into rollups.
	rec = s.postEnvelope(t, projectID, key, sessionAggregatesEnvelopeTest, "")
	if rec.Code != 200 {
		t.Fatalf("aggregates envelope = %d: %s", rec.Code, rec.Body.String())
	}
	_ = s.pool.QueryRow(t.Context(),
		`SELECT total, crashed FROM session_rollups WHERE project_id = $1`, projectID).Scan(&r.Total, &r.Crashed)
	if r.Total != 47 || r.Crashed != 3 { // 1 individual + 46 aggregate; 1 + 2 crashed
		t.Fatalf("aggregate rollup = %+v", r)
	}

	// Feedback → 200, stored.
	rec = s.postEnvelope(t, projectID, key, feedbackEnvelopeTest, "")
	if rec.Code != 200 {
		t.Fatalf("feedback envelope = %d: %s", rec.Code, rec.Body.String())
	}
	var fb struct{ Message string }
	if err := s.pool.QueryRow(t.Context(), `SELECT message FROM feedbacks WHERE project_id = $1`, projectID).Scan(&fb.Message); err != nil || fb.Message != "The save button did nothing" {
		t.Fatalf("feedback = %+v err=%v", fb, err)
	}

	// client_report + unknown item types → tolerated, counted, not stored as events.
	rec = s.postEnvelope(t, projectID, key, clientReportWithUnknownTest, "")
	if rec.Code != 200 {
		t.Fatalf("client report envelope = %d: %s", rec.Code, rec.Body.String())
	}

	// Implicit release created by first event declaring it.
	var releaseCount int
	_ = s.pool.QueryRow(t.Context(), `SELECT count(*) FROM releases WHERE project_id = $1 AND version = 'app@1.0.0'`, projectID).Scan(&releaseCount)
	if releaseCount != 1 {
		t.Fatalf("implicit release missing (%d)", releaseCount)
	}

	// Gzipped envelope → accepted.
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(errorEnvelopeTest))
	_ = zw.Close()
	rec = s.postEnvelope(t, projectID, key, buf.String(), "gzip")
	if rec.Code != 200 {
		t.Fatalf("gzipped envelope = %d: %s", rec.Code, rec.Body.String())
	}

	// Bad key → 403.
	rec = s.postEnvelope(t, projectID, "00000000-0000-0000-0000-000000000000", errorEnvelopeTest, "")
	if rec.Code != 403 {
		t.Fatalf("bad key = %d (want 403)", rec.Code)
	}

	// Malformed envelope → 400.
	rec = s.postEnvelope(t, projectID, key, "not an envelope", "")
	if rec.Code != 400 {
		t.Fatalf("malformed = %d (want 400)", rec.Code)
	}
}

func TestStoreEndpointLegacy(t *testing.T) {
	s := newTestServer(t)
	projectID, key := ingestSetup(t, s)

	body := `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc","timestamp":"2026-09-06T10:00:00Z","platform":"javascript","message":"legacy"}`
	rec := s.apiCall(t, "POST", "/api/"+projectID+"/store/?sentry_key="+key, body, jar{}, nil)
	if rec.Code != 200 {
		t.Fatalf("store = %d: %s", rec.Code, rec.Body.String())
	}
	var n int
	_ = s.pool.QueryRow(t.Context(), `SELECT count(*) FROM events_part WHERE project_id = $1`, projectID).Scan(&n)
	if n != 1 {
		t.Fatalf("store event not persisted (%d)", n)
	}
}

const errorEnvelopeTest = `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc","sent_at":"2026-09-06T10:00:00.000Z","sdk":{"name":"sentry.javascript.browser","version":"10.70.0"}}
{"type":"event"}
{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc","timestamp":"2026-09-06T10:00:00.000Z","platform":"javascript","level":"error","environment":"production","release":"app@1.0.0","tags":{"handled":"no"},"exception":{"values":[{"type":"TypeError","value":"Cannot read properties of undefined"}]}}`

const transactionEnvelopeTest = `{"event_id":"743ad8bbfdd84e99bc38b4729e2864de","sent_at":"2026-09-06T10:00:01.000Z","sdk":{"name":"sentry.javascript.browser","version":"10.70.0"}}
{"type":"transaction","event_id":"743ad8bbfdd84e99bc38b4729e2864de"}
{"type":"transaction","event_id":"743ad8bbfdd84e99bc38b4729e2864de","transaction":"/users/:id","transaction_info":{"source":"route"},"start_timestamp":1788220800.0,"timestamp":1788220801.234,"contexts":{"trace":{"trace_id":"743ad8bbfdd84e99bc38b4729e2864de","span_id":"a0cfbde2bdff3adc","op":"pageload","status":"ok"}},"measurements":{"lcp":{"value":2049,"unit":"millisecond"}}}`

const sessionEnvelopeTest = `{"sent_at":"2026-09-06T10:00:02.000Z","sdk":{"name":"sentry.javascript.browser","version":"10.70.0"}}
{"type":"session"}
{"sid":"7c7b6585-f901-4351-bf8d-02711b721929","init":true,"started":"2026-09-06T10:00:00.000Z","timestamp":"2026-09-06T10:02:10.000Z","status":"crashed","errors":1,"duration":130,"attrs":{"release":"app@1.0.0","environment":"production"}}`

const sessionAggregatesEnvelopeTest = `{"sent_at":"2026-09-06T11:00:00.000Z"}
{"type":"sessions"}
{"aggregates":[{"started":"2026-09-06T10:30:00.000Z","exited":40,"crashed":2,"abnormal":1,"errored":3}],"attrs":{"release":"app@1.0.0","environment":"production"}}`

const feedbackEnvelopeTest = `{"event_id":"d5f8d0e2a2c14d8e9f1a2b3c4d5e6f70","sent_at":"2026-09-06T10:00:03.000Z"}
{"type":"feedback"}
{"event_id":"d5f8d0e2a2c14d8e9f1a2b3c4d5e6f70","timestamp":"2026-09-06T10:00:03.000Z","platform":"javascript","contexts":{"feedback":{"message":"The save button did nothing","contact_email":"end@user.io","name":"End User","associated_event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}}}`

const clientReportWithUnknownTest = `{"sent_at":"2026-09-06T10:00:04.000Z"}
{"type":"client_report"}
{"timestamp":"2026-09-06T10:00:04.000Z","discarded_events":[{"reason":"queue_overflow","category":"error","quantity":23}]}
{"type":"future_item_type_v99"}
{"whatever":"goes"}
{"type":"attachment","length":5}
bytes`
