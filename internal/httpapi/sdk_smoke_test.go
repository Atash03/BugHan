package httpapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestSDKSmokeErrorTransactionSession is the automated SDK smoke suite
// (DESIGN.md §5, T12): sentry-js-shaped envelopes — one error, one
// transaction carrying web vitals + spans, one session — flow through the
// live HTTP surface and must be visible via the REST API: issues list,
// issue events, performance summary, transaction detail, trace view, and
// release health. The manual companion (real @sentry/browser against Vite +
// webpack hello-world apps) lives in docs/sdk-smoke.md.
func TestSDKSmokeErrorTransactionSession(t *testing.T) {
	s := newTestServer(t)
	fx := apiSetup(t, s)

	now := time.Now().UTC().Truncate(time.Second)
	ts := now.Format("2006-01-02T15:04:05.000Z")
	startUnix := float64(now.Add(-1300 * time.Millisecond).UnixMilli()) / 1000
	endUnix := float64(now.UnixMilli()) / 1000

	const (
		errorID = "aaaaaaaa-1111-4111-8111-111111111111"
		txID    = "bbbbbbbb-2222-4222-8222-222222222222"
		traceID = "cccccccc-3333-4333-8333-333333333333"
		spanID  = "dd00112233445566"
		release = "smoke@1.0.0"
		txName  = "/smoke"
	)

	errorEnv := fmt.Sprintf(`{"event_id":"%s","sent_at":"%s","sdk":{"name":"sentry.javascript.browser","version":"10.70.0"}}
{"type":"event"}
{"event_id":"%s","timestamp":"%s","platform":"javascript","level":"error","environment":"production","release":"%s","tags":{"handled":"no"},"exception":{"values":[{"type":"TypeError","value":"smoke boom","stacktrace":{"frames":[{"filename":"app.js","function":"onClick","lineno":10,"colno":5,"in_app":true}]}}]},"contexts":{"trace":{"trace_id":"%s","span_id":"%s","op":"pageload"}}}`,
		errorID, ts, errorID, ts, release, traceID, spanID)

	txEnv := fmt.Sprintf(`{"event_id":"%s","sent_at":"%s","sdk":{"name":"sentry.javascript.browser","version":"10.70.0"},"trace":{"trace_id":"%s","span_id":"%s","public_key":"%s"}}
{"type":"transaction"}
{"type":"transaction","event_id":"%s","transaction":"%s","transaction_info":{"source":"route"},"environment":"production","release":"%s","platform":"javascript","start_timestamp":%f,"timestamp":%f,"contexts":{"trace":{"trace_id":"%s","span_id":"%s","op":"pageload","status":"ok"}},"measurements":{"lcp":{"value":2049,"unit":"millisecond"},"cls":{"value":0.02,"unit":"none"}},"spans":[{"span_id":"ee00112233445566","parent_span_id":"%s","op":"db","description":"SELECT 1","status":"ok","start_timestamp":%f,"timestamp":%f}]}`,
		txID, ts, traceID, spanID, fx.Key, txID, txName, release, startUnix, endUnix, traceID, spanID, spanID, startUnix, endUnix)

	sessionEnv := fmt.Sprintf(`{"sent_at":"%s","sdk":{"name":"sentry.javascript.browser","version":"10.70.0"}}
{"type":"session"}
{"sid":"ff001122-3344-4566-8788-99aabbccddee","init":true,"started":"%s","timestamp":"%s","status":"ok","errors":0,"duration":12.5,"attrs":{"release":"%s","environment":"production"}}`,
		ts, ts, ts, release)

	for name, body := range map[string]string{
		"error":       errorEnv,
		"transaction": txEnv,
		"session":     sessionEnv,
	} {
		rec := s.postEnvelope(t, fx.ProjectID, fx.Key, body, "")
		if rec.Code != 200 {
			t.Fatalf("%s envelope = %d: %s", name, rec.Code, rec.Body.String())
		}
	}
	// (postEnvelope sends sentry.javascript.browser/10.70.0 as the client,
	// matching the sdk blocks above.)

	// 1. The error groups into a visible issue.
	rec := s.apiCall(t, "GET", fx.issuesPath(), "", jar{}, fx.Bearer)
	if rec.Code != 200 {
		t.Fatalf("issues list = %d: %s", rec.Code, rec.Body.String())
	}
	var issues issueListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &issues); err != nil {
		t.Fatalf("issues decode: %v", err)
	}
	if issues.Meta.Total < 1 || len(issues.Data) == 0 {
		t.Fatalf("smoke error produced no issue: %s", rec.Body.String())
	}
	title, _ := issues.Data[0]["title"].(string)
	if !strings.Contains(title, "smoke boom") {
		t.Fatalf("issue title = %q, want the smoke error", title)
	}
	issueID, _ := issues.Data[0]["id"].(string)

	// 2. The issue carries the ingested event.
	rec = s.apiCall(t, "GET", fx.issuePath(issueID)+"events/", "", jar{}, fx.Bearer)
	if rec.Code != 200 {
		t.Fatalf("issue events = %d: %s", rec.Code, rec.Body.String())
	}
	var events struct {
		Data []map[string]any `json:"data"`
		Meta struct {
			Total int `json:"total"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &events); err != nil || events.Meta.Total < 1 {
		t.Fatalf("issue events missing: %s (%v)", rec.Body.String(), err)
	}

	// 3. The transaction appears in the performance summary with vitals.
	rec = s.apiCall(t, "GET", "/api/0/projects/"+fx.OrgSlug+"/"+fx.ProjectSlug+"/performance/summary/", "", jar{}, fx.Bearer)
	if rec.Code != 200 {
		t.Fatalf("performance summary = %d: %s", rec.Code, rec.Body.String())
	}
	var summary []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("summary decode: %v", err)
	}
	var foundTx bool
	for _, row := range summary {
		if row["name"] == txName {
			foundTx = true
			if c, _ := row["count"].(float64); c < 1 {
				t.Fatalf("summary count = %v for %s", row["count"], txName)
			}
		}
	}
	if !foundTx {
		t.Fatalf("transaction %q missing from summary: %s", txName, rec.Body.String())
	}

	// 4. Transaction detail exposes the recent event with LCP.
	rec = s.apiCall(t, "GET", "/api/0/projects/"+fx.OrgSlug+"/"+fx.ProjectSlug+"/performance/transaction/?name="+txName, "", jar{}, fx.Bearer)
	if rec.Code != 200 {
		t.Fatalf("transaction detail = %d: %s", rec.Code, rec.Body.String())
	}
	var detail struct {
		Name   string `json:"name"`
		Count  int64  `json:"count"`
		Recent []struct {
			ID     string             `json:"id"`
			Vitals map[string]float64 `json:"vitals"`
		} `json:"recent"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil || detail.Count < 1 {
		t.Fatalf("transaction detail missing: %s (%v)", rec.Body.String(), err)
	}
	if len(detail.Recent) == 0 {
		t.Fatalf("transaction detail has no recent events: %s", rec.Body.String())
	}
	if _, ok := detail.Recent[0].Vitals["lcp"]; !ok {
		t.Fatalf("LCP vital lost: %+v", detail.Recent[0].Vitals)
	}

	// 5. The trace view links the transaction, its span, and the error.
	rec = s.apiCall(t, "GET", "/api/0/projects/"+fx.OrgSlug+"/"+fx.ProjectSlug+"/traces/"+traceID+"/", "", jar{}, fx.Bearer)
	if rec.Code != 200 {
		t.Fatalf("trace = %d: %s", rec.Code, rec.Body.String())
	}
	var trace struct {
		TraceID      string `json:"trace_id"`
		Transactions []struct {
			Name  string `json:"name"`
			Spans []struct {
				Op string `json:"op"`
			} `json:"spans"`
		} `json:"transactions"`
		Errors []struct {
			Title string `json:"title"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &trace); err != nil {
		t.Fatalf("trace decode: %v", err)
	}
	if len(trace.Transactions) != 1 || trace.Transactions[0].Name != txName {
		t.Fatalf("trace transactions = %+v, want [%s]", trace.Transactions, txName)
	}
	if len(trace.Transactions[0].Spans) == 0 || trace.Transactions[0].Spans[0].Op != "db" {
		t.Fatalf("trace spans = %+v, want the db child span", trace.Transactions[0].Spans)
	}
	if len(trace.Errors) != 1 || !strings.Contains(trace.Errors[0].Title, "smoke boom") {
		t.Fatalf("trace errors = %+v, want the smoke error tick", trace.Errors)
	}

	// 6. Release health reflects the session (crash-free, adopted release).
	rec = s.apiCall(t, "GET", "/api/0/projects/"+fx.OrgSlug+"/"+fx.ProjectSlug+"/releases-health/", "", jar{}, fx.Bearer)
	if rec.Code != 200 {
		t.Fatalf("releases health = %d: %s", rec.Code, rec.Body.String())
	}
	var health []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &health); err != nil {
		t.Fatalf("health decode: %v", err)
	}
	var foundRelease bool
	for _, row := range health {
		if row["release"] == release {
			foundRelease = true
			if tot, _ := row["total"].(float64); tot < 1 {
				t.Fatalf("release health total = %v", row["total"])
			}
			if cf, _ := row["crash_free_sessions"].(float64); cf != 1 {
				t.Fatalf("crash_free_sessions = %v, want 1 for the ok session", cf)
			}
		}
	}
	if !foundRelease {
		t.Fatalf("release %q missing from health: %s", release, rec.Body.String())
	}

	// 7. Liveness stays green after the whole flow.
	rec = s.get(t, "/api/health/", jar{}, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("health after smoke = %d: %s", rec.Code, rec.Body.String())
	}
}
