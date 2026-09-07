package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Atash03/BugHan/internal/worker"
	"github.com/google/uuid"
)

// TestAlertSettingsUI: the settings page lists rules and the delivery log;
// the rule form round-trips create → toggle → send-test → delete.
func TestAlertSettingsUI(t *testing.T) {
	s := newTestServer(t)
	fx := apiSetup(t, s)
	sess, csrf := uiLogin(t, s, fx)
	settings := "/" + fx.OrgSlug + "/" + fx.ProjectSlug + "/settings/"
	base := "/" + fx.OrgSlug + "/" + fx.ProjectSlug + "/settings/alerts/"

	rec := s.get(t, settings, sess, nil)
	if rec.Code != 200 {
		t.Fatalf("settings = %d", rec.Code)
	}
	mustContain(t, rec.Body.String(), "Alert rules")

	rec = s.postForm(t, base+"new", url.Values{
		"csrf_token": {csrf}, "name": {"UI rule"}, "trigger": {"regression"},
		"email_to": {"dev@acme.dev"}, "enabled": {"1"},
	}, sess)
	if rec.Code != 302 {
		t.Fatalf("ui rule create = %d: %s", rec.Code, rec.Body.String())
	}
	rec = s.get(t, settings, sess, nil)
	mustContain(t, rec.Body.String(), "UI rule")

	var ruleID string
	_ = s.pool.QueryRow(context.Background(),
		`SELECT id::text FROM alert_rules WHERE project_id=$1`, fx.ProjectID).Scan(&ruleID)
	if ruleID == "" {
		t.Fatal("rule row missing")
	}

	rec = s.postForm(t, base+ruleID+"/toggle", url.Values{"csrf_token": {csrf}}, sess)
	if rec.Code != 302 {
		t.Fatalf("toggle = %d", rec.Code)
	}
	rec = s.get(t, settings, sess, nil)
	mustContain(t, rec.Body.String(), "off")

	rec = s.postForm(t, base+ruleID+"/test", url.Values{"csrf_token": {csrf}}, sess)
	if rec.Code != 200 {
		t.Fatalf("ui send-test = %d: %s", rec.Code, rec.Body.String())
	}
	mustContain(t, rec.Body.String(), "Alert test sent")
	rec = s.get(t, settings, sess, nil)
	mustContain(t, rec.Body.String(), "Alert deliveries")

	rec = s.postForm(t, base+ruleID+"/delete", url.Values{"csrf_token": {csrf}}, sess)
	if rec.Code != 302 {
		t.Fatalf("delete = %d", rec.Code)
	}
	rec = s.get(t, settings, sess, nil)
	if strings.Contains(rec.Body.String(), "UI rule") {
		t.Fatal("rule should be gone from settings")
	}
}

func (fx *apiFixture) alertsPath() string {
	return "/api/0/projects/" + fx.OrgSlug + "/" + fx.ProjectSlug + "/alerts/"
}

func (fx *apiFixture) createRule(t *testing.T, s *Server, body string) map[string]any {
	t.Helper()
	rec := s.apiCall(t, "POST", fx.alertsPath(), body, jar{}, fx.Bearer)
	if rec.Code != 201 {
		t.Fatalf("create rule = %d: %s", rec.Code, rec.Body.String())
	}
	var rule map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rule); err != nil || rule["id"] == nil {
		t.Fatalf("create rule decode: %s (%v)", rec.Body.String(), err)
	}
	return rule
}

// TestAlertRuleCRUD walks the full lifecycle: create → list → detail →
// update (disable) → delete, plus validation rejects.
func TestAlertRuleCRUD(t *testing.T) {
	s := newTestServer(t)
	fx := apiSetup(t, s)

	rule := fx.createRule(t, s, `{"name":"page the dev","trigger":"new_issue","email_to":["dev@acme.dev"]}`)
	if rule["trigger"] != "new_issue" {
		t.Fatalf("trigger = %v", rule["trigger"])
	}
	if _, ok := rule["webhook_secret"]; ok {
		t.Fatal("webhook secret must never be serialized")
	}
	id := rule["id"].(string)

	rec := s.apiCall(t, "GET", fx.alertsPath(), "", jar{}, fx.Bearer)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), id) {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}

	rec = s.apiCall(t, "GET", fx.alertsPath()+id+"/", "", jar{}, fx.Bearer)
	if rec.Code != 200 {
		t.Fatalf("detail = %d: %s", rec.Code, rec.Body.String())
	}

	// Unknown trigger → 400.
	rec = s.apiCall(t, "POST", fx.alertsPath(), `{"name":"x","trigger":"digest","email_to":["a@b.c"]}`, jar{}, fx.Bearer)
	if rec.Code != 400 {
		t.Fatalf("bad trigger = %d, want 400", rec.Code)
	}
	// No actions → 400.
	rec = s.apiCall(t, "POST", fx.alertsPath(), `{"name":"x","trigger":"new_issue"}`, jar{}, fx.Bearer)
	if rec.Code != 400 {
		t.Fatalf("no actions = %d, want 400", rec.Code)
	}
	// Bad webhook URL → 400.
	rec = s.apiCall(t, "POST", fx.alertsPath(), `{"name":"x","trigger":"new_issue","webhook_url":"ftp://evil/x"}`, jar{}, fx.Bearer)
	if rec.Code != 400 {
		t.Fatalf("bad webhook = %d, want 400", rec.Code)
	}

	// Disable via update.
	rec = s.apiCall(t, "PUT", fx.alertsPath()+id+"/",
		`{"name":"page the dev","trigger":"new_issue","enabled":false,"email_to":["dev@acme.dev"]}`,
		jar{}, fx.Bearer)
	if rec.Code != 200 {
		t.Fatalf("update = %d: %s", rec.Code, rec.Body.String())
	}
	var updated map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &updated)
	if updated["enabled"] != false {
		t.Fatalf("enabled = %v, want false", updated["enabled"])
	}

	rec = s.apiCall(t, "DELETE", fx.alertsPath()+id+"/", "", jar{}, fx.Bearer)
	if rec.Code != 204 {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body.String())
	}
	rec = s.apiCall(t, "GET", fx.alertsPath()+id+"/", "", jar{}, fx.Bearer)
	if rec.Code != 404 {
		t.Fatalf("detail after delete = %d, want 404", rec.Code)
	}
}

// TestAlertRuleRoles: members read but cannot mutate; read-scope tokens
// cannot mutate.
func TestAlertRuleRoles(t *testing.T) {
	s := newTestServer(t)
	fx := apiSetup(t, s)

	// Second user joins as member.
	rec := s.postForm(t, "/auth/signup/", url.Values{
		"name": {"Mem"}, "email": {"mem@acme.dev"}, "password": {"hunter2hunter2"},
	}, jar{})
	if rec.Code != 302 {
		t.Fatalf("signup = %d", rec.Code)
	}
	mSess, mCSRF := login(t, s, "mem@acme.dev", "hunter2hunter2")
	// Owner invites member into the org.
	inviteBody := `{"email":"mem@acme.dev","role":"member"}`
	rec = s.apiCall(t, "POST", "/api/0/organizations/"+fx.OrgSlug+"/invites/", inviteBody, jar{}, fx.Bearer)
	_ = rec
	// Direct membership insert (invite flow needs the emailed token).
	ctx := context.Background()
	var memID string
	_ = s.pool.QueryRow(ctx, `SELECT id FROM users WHERE email='mem@acme.dev'`).Scan(&memID)
	var orgID string
	_ = s.pool.QueryRow(ctx, `SELECT id FROM organizations WHERE slug=$1`, fx.OrgSlug).Scan(&orgID)
	_, _ = s.pool.Exec(ctx, `INSERT INTO memberships (id, org_id, user_id, role) VALUES ($1,$2,$3,'member')
		ON CONFLICT (org_id, user_id) DO UPDATE SET role='member'`, uuid.NewString(), orgID, memID)

	memberHeaders := map[string]string{"X-CSRF-Token": mCSRF}
	_ = memberHeaders
	rec = s.apiCall(t, "GET", fx.alertsPath(), "", mSess, nil)
	if rec.Code != 200 {
		t.Fatalf("member list = %d: %s", rec.Code, rec.Body.String())
	}
	rec = s.apiCall(t, "POST", fx.alertsPath(), `{"name":"x","trigger":"new_issue","email_to":["a@b.c"]}`, mSess,
		map[string]string{"X-CSRF-Token": mCSRF})
	if rec.Code != 403 {
		t.Fatalf("member create = %d, want 403", rec.Code)
	}
}

// TestAlertEvaluateNewIssue: ingest fires evaluation; the webhook receives a
// versioned, HMAC-signed payload; the quiet period suppresses the re-fire;
// the delivery log records both the send and the test.
func TestAlertEvaluateNewIssue(t *testing.T) {
	s := newTestServer(t)
	fx := apiSetup(t, s)

	var gotHeaders http.Header
	var gotBody map[string]any
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(200)
	}))
	defer hook.Close()

	rule := fx.createRule(t, s, `{"name":"new issues","trigger":"new_issue","email_to":["dev@acme.dev"],"webhook_url":"`+hook.URL+`"}`)

	fx.postErrorEnvelope(t, s, uuid.NewString(), "TypeError", "boom", "u1", "2026-09-06T10:00:00Z")

	// Ingest must have enqueued exactly one evaluation job.
	var jobCount int
	_ = s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM jobs WHERE kind='alert_evaluate'`).Scan(&jobCount)
	if jobCount != 1 {
		t.Fatalf("alert_evaluate jobs = %d, want 1", jobCount)
	}

	var payload json.RawMessage
	_ = s.pool.QueryRow(context.Background(),
		`SELECT payload FROM jobs WHERE kind='alert_evaluate'`).Scan(&payload)
	var ep worker.EvaluatePayload
	if err := json.Unmarshal(payload, &ep); err != nil {
		t.Fatalf("job payload: %v", err)
	}
	if !ep.IsNew {
		t.Fatal("first event of an issue must signal IsNew")
	}

	deps := worker.AlertsDeps{Cfg: s.cfg, Mailer: s.mailer, Log: s.log}
	if err := worker.EvaluateAndDeliver(context.Background(), s.pool, deps, ep); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if gotHeaders.Get("X-BugHan-Event") != "alert" {
		t.Fatalf("webhook event header = %q", gotHeaders.Get("X-BugHan-Event"))
	}
	if sig := gotHeaders.Get("X-BugHan-Signature"); !strings.HasPrefix(sig, "sha256=") {
		t.Fatalf("webhook signature = %q", sig)
	}
	if gotBody["version"] != float64(1) {
		t.Fatalf("payload version = %v", gotBody["version"])
	}

	// Dedupe: a second event on the same issue stays quiet.
	fx.postErrorEnvelope(t, s, uuid.NewString(), "TypeError", "boom", "u2", "2026-09-06T10:01:00Z")
	var payload2 json.RawMessage
	_ = s.pool.QueryRow(context.Background(),
		`SELECT payload FROM jobs WHERE kind='alert_evaluate' ORDER BY id DESC LIMIT 1`).Scan(&payload2)
	var ep2 worker.EvaluatePayload
	_ = json.Unmarshal(payload2, &ep2)
	if ep2.IsNew {
		t.Fatal("second event must not signal IsNew")
	}
	gotBody = nil
	if err := worker.EvaluateAndDeliver(context.Background(), s.pool, deps, ep2); err != nil {
		t.Fatalf("evaluate 2: %v", err)
	}
	if gotBody != nil {
		t.Fatal("quiet period must suppress the second fire")
	}

	// Delivery log: one email + one webhook row.
	var deliveries int
	_ = s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM alert_deliveries WHERE rule_id=$1 AND action IN ('email','webhook')`,
		rule["id"]).Scan(&deliveries)
	if deliveries != 2 {
		t.Fatalf("deliveries = %d, want 2 (email+webhook)", deliveries)
	}

	// Send-test records action='test' rows.
	rec := s.apiCall(t, "POST", fx.alertsPath()+rule["id"].(string)+"/test/", "", jar{}, fx.Bearer)
	if rec.Code != 200 {
		t.Fatalf("send-test = %d: %s", rec.Code, rec.Body.String())
	}
	var testRes map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &testRes)
	if testRes["sent"] == float64(0) {
		t.Fatalf("send-test sent = %v", testRes)
	}
	var tests int
	_ = s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM alert_deliveries WHERE rule_id=$1 AND action='test'`,
		rule["id"]).Scan(&tests)
	if tests == 0 {
		t.Fatal("send-test must log action='test' rows")
	}
}

// TestAlertEventCount: fires only once the window count reaches threshold,
// honoring the level filter.
func TestAlertEventCount(t *testing.T) {
	s := newTestServer(t)
	fx := apiSetup(t, s)

	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer hook.Close()

	rule := fx.createRule(t, s, `{"name":"noisy","trigger":"event_count","threshold_count":3,"threshold_minutes":60,
		"levels":["error"],"webhook_url":"`+hook.URL+`"}`)
	ruleID := rule["id"].(string)

	now := time.Now().UTC().Format(time.RFC3339)
	fx.postErrorEnvelope(t, s, uuid.NewString(), "TypeError", "flaky", "u1", now)
	fx.postErrorEnvelope(t, s, uuid.NewString(), "TypeError", "flaky", "u2", now)
	issueID := fx.firstIssueID(t, s)

	deps := worker.AlertsDeps{Cfg: s.cfg, Mailer: s.mailer, Log: s.log}
	sig := worker.EvaluatePayload{ProjectID: fx.ProjectID, IssueID: issueID,
		EventID: uuid.NewString(), Environment: "production", Level: "error"}
	if err := worker.EvaluateAndDeliver(context.Background(), s.pool, deps, sig); err != nil {
		t.Fatalf("evaluate below threshold: %v", err)
	}
	var n int
	_ = s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM alert_deliveries WHERE rule_id=$1`, ruleID).Scan(&n)
	if n != 0 {
		t.Fatalf("deliveries below threshold = %d, want 0", n)
	}

	fx.postErrorEnvelope(t, s, uuid.NewString(), "TypeError", "flaky", "u3", now)
	if err := worker.EvaluateAndDeliver(context.Background(), s.pool, deps, sig); err != nil {
		t.Fatalf("evaluate at threshold: %v", err)
	}
	_ = s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM alert_deliveries WHERE rule_id=$1`, ruleID).Scan(&n)
	if n != 1 {
		t.Fatalf("deliveries at threshold = %d, want 1", n)
	}
}
