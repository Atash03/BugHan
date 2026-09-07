package worker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/Atash03/BugHan/internal/alerts"
	"github.com/Atash03/BugHan/internal/config"
	"github.com/Atash03/BugHan/internal/mail"
	"github.com/jackc/pgx/v5/pgxpool"
)

// JobKindAlertEvaluate fires after an error event is stored: it loads the
// project's enabled rules, applies filters/triggers/dedupe, and delivers.
const JobKindAlertEvaluate = "alert_evaluate"

// EvaluatePayload is the alert_evaluate job body.
type EvaluatePayload struct {
	ProjectID    string `json:"project_id"`
	IssueID      string `json:"issue_id"`
	EventID      string `json:"event_id"`
	IsNew        bool   `json:"is_new"`
	IsRegression bool   `json:"is_regression"`
	Environment  string `json:"environment"`
	Release      string `json:"release"`
	Level        string `json:"level"`
}

// AlertsDeps carries delivery dependencies (SMTP, public URL for links).
type AlertsDeps struct {
	Cfg    *config.Config
	Mailer *mail.Mailer
	Client *http.Client // nil → default 5s-timeout client
	Log    *slog.Logger
}

// NewSecret mints a per-rule webhook HMAC secret (32 hex chars).
func NewSecret() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// RegisterAlerts wires alert evaluation into the worker. Delivery retries
// ride on the jobs table itself (max_attempts=3 with backoff in execute);
// per-action outcomes are recorded in alert_deliveries either way.
func (w *Worker) RegisterAlerts(d AlertsDeps) {
	if d.Client == nil {
		d.Client = &http.Client{Timeout: 5 * time.Second}
	}
	w.Register(JobKindAlertEvaluate, func(ctx context.Context, raw json.RawMessage) error {
		var p EvaluatePayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return fmt.Errorf("alert payload: %w", err)
		}
		return EvaluateAndDeliver(ctx, w.pool, d, p)
	})
}

// EvaluateAndDeliver runs one signal through every enabled rule of its
// project. Unexpected DB errors return err (job retry); per-action delivery
// failures are recorded in alert_deliveries and do not fail the job.
func EvaluateAndDeliver(ctx context.Context, pool *pgxpool.Pool, d AlertsDeps, p EvaluatePayload) error {
	ruleRows, err := loadEnabledRules(ctx, pool, p.ProjectID)
	if err != nil {
		return err
	}
	if len(ruleRows) == 0 {
		return nil
	}
	issue, err := loadIssueBrief(ctx, pool, p.IssueID)
	if err != nil {
		return err
	}
	proj, err := loadProjectBrief(ctx, pool, p.ProjectID)
	if err != nil {
		return err
	}

	sig := alerts.Signal{
		ProjectID: p.ProjectID, IssueID: p.IssueID, EventID: p.EventID,
		IsNew: p.IsNew, IsRegression: p.IsRegression,
		Environment: p.Environment, Release: p.Release, Level: p.Level,
	}
	for _, r := range ruleRows {
		var windowCount int64
		if r.Trigger == "event_count" {
			mins := r.ThresholdMinutes
			if mins <= 0 {
				mins = 60
			}
			if err := pool.QueryRow(ctx, `
				SELECT count(*) FROM events_part
				WHERE issue_id = $1 AND timestamp > now() - ($2 || ' minutes')::interval`,
				p.IssueID, fmt.Sprint(mins)).Scan(&windowCount); err != nil {
				return err
			}
		}
		if !r.ShouldSend(sig, windowCount) {
			continue
		}
		if deduped, err := checkDedupe(ctx, pool, r.ID, p.IssueID, r.QuietMinutes); err != nil {
			return err
		} else if deduped {
			continue
		}
		deliverRule(ctx, pool, d, r, proj, issue, p, false)
		if err := touchDedupe(ctx, pool, r.ID, p.IssueID); err != nil {
			return err
		}
	}
	return nil
}

type ruleRow = alerts.Rule

// loadEnabledRules reads a project's enabled rules.
func loadEnabledRules(ctx context.Context, pool *pgxpool.Pool, projectID string) ([]*alerts.Rule, error) {
	rows, err := pool.Query(ctx, `
		SELECT id::text, project_id::text, COALESCE(name, ''), trigger, enabled,
		       COALESCE(environments, '{}'), COALESCE(releases, '{}'), COALESCE(levels, '{}'),
		       threshold_count, threshold_minutes, quiet_minutes,
		       COALESCE(email_to, '{}'), COALESCE(webhook_url, ''), COALESCE(webhook_secret, '')
		FROM alert_rules WHERE project_id = $1 AND enabled ORDER BY created_at`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*alerts.Rule{}
	for rows.Next() {
		var r alerts.Rule
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.Name, &r.Trigger, &r.Enabled,
			&r.Environments, &r.Releases, &r.Levels,
			&r.ThresholdCount, &r.ThresholdMinutes, &r.QuietMinutes,
			&r.EmailTo, &r.WebhookURL, &r.WebhookSecret); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// issueBrief / projectBrief are the link-building facts for payloads.
type issueBrief struct {
	ID, Title, Culprit, Level, Status, Substatus string
	Count                                        int64
}

type projectBrief struct {
	ID, Name, Slug, OrgSlug, OrgName string
}

func loadIssueBrief(ctx context.Context, pool *pgxpool.Pool, issueID string) (*issueBrief, error) {
	var b issueBrief
	err := pool.QueryRow(ctx, `
		SELECT id::text, title, culprit, level, status, substatus, count
		FROM issues WHERE id = $1`, issueID).Scan(
		&b.ID, &b.Title, &b.Culprit, &b.Level, &b.Status, &b.Substatus, &b.Count)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func loadProjectBrief(ctx context.Context, pool *pgxpool.Pool, projectID string) (*projectBrief, error) {
	var b projectBrief
	err := pool.QueryRow(ctx, `
		SELECT p.id::text, p.name, p.slug, o.slug, o.name
		FROM projects p JOIN organizations o ON o.id = p.org_id
		WHERE p.id = $1`, projectID).Scan(&b.ID, &b.Name, &b.Slug, &b.OrgSlug, &b.OrgName)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// checkDedupe reports whether the (rule, issue) pair fired within the quiet
// period (default 30 min; <=0 means always send).
func checkDedupe(ctx context.Context, pool *pgxpool.Pool, ruleID, issueID string, quietMinutes int) (bool, error) {
	if quietMinutes <= 0 {
		return false, nil
	}
	var last string
	err := pool.QueryRow(ctx, `
		SELECT last_sent_at::text FROM alert_dedupe WHERE rule_id = $1 AND issue_id = $2`,
		ruleID, issueID).Scan(&last)
	if err != nil {
		return false, nil // no row → never sent
	}
	var mins float64
	err = pool.QueryRow(ctx, `SELECT EXTRACT(EPOCH FROM (now() - $1::timestamptz))/60`,
		last).Scan(&mins)
	if err != nil {
		return false, nil
	}
	return mins < float64(quietMinutes), nil
}

func touchDedupe(ctx context.Context, pool *pgxpool.Pool, ruleID, issueID string) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO alert_dedupe (rule_id, issue_id, last_sent_at)
		VALUES ($1, $2, now())
		ON CONFLICT (rule_id, issue_id) DO UPDATE SET last_sent_at = now()`,
		ruleID, issueID)
	return err
}

// payloadMaps shapes the versioned contract (rule/project/org/issue/event +
// URLs) shared by webhook bodies and email text.
func payloadMaps(publicURL string, r *alerts.Rule, proj *projectBrief, issue *issueBrief, p EvaluatePayload, isTest bool) (rule, project, iss, urls map[string]any) {
	event := alerts.EventAlert
	if isTest {
		event = alerts.EventTest
	}
	_ = event
	rule = map[string]any{"id": r.ID, "name": r.Name, "trigger": r.Trigger}
	project = map[string]any{"id": proj.ID, "name": proj.Name, "slug": proj.Slug,
		"organization": proj.OrgSlug}
	iss = map[string]any{"id": issue.ID, "title": issue.Title, "culprit": issue.Culprit,
		"level": issue.Level, "status": issue.Status, "substatus": issue.Substatus,
		"count": issue.Count, "event_id": p.EventID,
		"environment": p.Environment, "release": p.Release}
	base := publicURL + "/" + proj.OrgSlug + "/" + proj.Slug
	urls = map[string]any{
		"issue": base + "/issues/" + issue.ID + "/",
		"event": base + "/issues/" + issue.ID + "/?event=" + p.EventID,
	}
	return rule, project, iss, urls
}

// deliverRule sends one rule's actions and records each outcome. Test sends
// use the alert-test event name and action='test' so they read clearly in
// the delivery log. Returns sent/failed action counts.
func deliverRule(ctx context.Context, pool *pgxpool.Pool, d AlertsDeps, r *alerts.Rule, proj *projectBrief, issue *issueBrief, p EvaluatePayload, isTest bool) (sent, failed int) {
	event := alerts.EventAlert
	emailAction, webhookAction := "email", "webhook"
	if isTest {
		event = alerts.EventTest
		emailAction, webhookAction = "test", "test"
	}
	ruleMap, projMap, issMap, urls := payloadMaps(publicBase(d), r, proj, issue, p, isTest)
	body, _ := json.Marshal(alerts.BuildPayload(event, ruleMap, projMap, issMap, r.Trigger, urls))

	if len(r.EmailTo) > 0 {
		subject := "[BugHan] " + r.Name + " — " + issue.Title
		if subject == "[BugHan]  — " {
			subject = "[BugHan] alert — " + issue.Title
		}
		text := emailText(publicBase(d), r, proj, issue, p, urls, isTest)
		for _, to := range r.EmailTo {
			err := d.Mailer.Send(to, subject, text)
			recordDelivery(ctx, pool, r.ID, p.ProjectID, p.IssueID, p.EventID,
				r.Trigger, emailAction, to, err)
			if err != nil {
				failed++
			} else {
				sent++
			}
		}
	}
	if r.WebhookURL != "" {
		err := SendWebhook(ctx, d.Client, r.WebhookURL, r.WebhookSecret, event, body)
		recordDelivery(ctx, pool, r.ID, p.ProjectID, p.IssueID, p.EventID,
			r.Trigger, webhookAction, r.WebhookURL, err)
		if err != nil {
			failed++
		} else {
			sent++
		}
	}
	return sent, failed
}

// SendTest fires a rule's actions immediately with a test payload: the
// project's latest issue/event when one exists, synthetic placeholders
// otherwise. Dedupe and triggers are bypassed; attempts land in the delivery
// log with action='test'.
func SendTest(ctx context.Context, pool *pgxpool.Pool, d AlertsDeps, ruleID, projectID string) (sent, failed int, err error) {
	var r alerts.Rule
	err = pool.QueryRow(ctx, `
		SELECT id::text, project_id::text, COALESCE(name, ''), trigger, enabled,
		       COALESCE(environments, '{}'), COALESCE(releases, '{}'), COALESCE(levels, '{}'),
		       threshold_count, threshold_minutes, quiet_minutes,
		       COALESCE(email_to, '{}'), COALESCE(webhook_url, ''), COALESCE(webhook_secret, '')
		FROM alert_rules WHERE id = $1 AND project_id = $2`,
		ruleID, projectID).Scan(&r.ID, &r.ProjectID, &r.Name, &r.Trigger, &r.Enabled,
		&r.Environments, &r.Releases, &r.Levels,
		&r.ThresholdCount, &r.ThresholdMinutes, &r.QuietMinutes,
		&r.EmailTo, &r.WebhookURL, &r.WebhookSecret)
	if err != nil {
		return 0, 0, fmt.Errorf("rule not found")
	}
	proj, err := loadProjectBrief(ctx, pool, projectID)
	if err != nil {
		return 0, 0, err
	}
	// Latest issue/event for realistic links; synthetic when the project is
	// still empty.
	issue := &issueBrief{ID: "test", Title: "Test alert", Culprit: "test", Level: "error"}
	p := EvaluatePayload{ProjectID: projectID, IssueID: "", EventID: "",
		Environment: "production", Release: "", Level: "error"}
	var issueID string
	if err := pool.QueryRow(ctx, `
		SELECT i.id::text, i.title, i.culprit, i.level
		FROM issues i WHERE i.project_id = $1 ORDER BY i.last_seen DESC LIMIT 1`,
		projectID).Scan(&issue.ID, &issue.Title, &issue.Culprit, &issue.Level); err == nil {
		issueID = issue.ID
		p.IssueID = issueID
		_ = pool.QueryRow(ctx, `
			SELECT e.id::text, e.environment, COALESCE(e.release, ''), e.level
			FROM events_part e WHERE e.issue_id = $1
			ORDER BY e.timestamp DESC LIMIT 1`, issueID).
			Scan(&p.EventID, &p.Environment, &p.Release, &p.Level)
		var count int64
		_ = pool.QueryRow(ctx, `SELECT count FROM issues WHERE id = $1`, issueID).Scan(&count)
		issue.Count = count
	}
	sent, failed = deliverRule(ctx, pool, d, &r, proj, issue, p, true)
	return sent, failed, nil
}

func publicBase(d AlertsDeps) string {
	if d.Cfg != nil && d.Cfg.PublicURL != "" {
		return d.Cfg.PublicURL
	}
	return "http://localhost:8000"
}

func emailText(base string, r *alerts.Rule, proj *projectBrief, issue *issueBrief, p EvaluatePayload, urls map[string]any, isTest bool) string {
	prefix := ""
	if isTest {
		prefix = "This is a TEST alert (no event fired it).\n\n"
	}
	return prefix + "Rule: " + r.Name + " (" + r.Trigger + ")\n" +
		"Project: " + proj.OrgSlug + " / " + proj.Slug + "\n" +
		"Issue: " + issue.Title + "\n" +
		" culprit: " + issue.Culprit + "\n" +
		" level: " + p.Level + "  env: " + p.Environment + "  release: " + p.Release + "\n" +
		" count: " + fmt.Sprint(issue.Count) + "\n" +
		"Issue: " + fmt.Sprint(urls["issue"]) + "\n" +
		"Payload version: " + fmt.Sprint(alerts.PayloadVersion) + "\n" +
		"Base URL: " + base + "\n"
}

func recordDelivery(ctx context.Context, pool *pgxpool.Pool, ruleID, projectID, issueID, eventID, trigger, action, target string, sendErr error) {
	status, errMsg := "sent", ""
	if sendErr != nil {
		status, errMsg = "failed", sendErr.Error()
	}
	var issueArg, eventArg any = issueID, eventID
	if issueID == "" {
		issueArg = nil
	}
	if eventID == "" {
		eventArg = nil
	}
	_, _ = pool.Exec(ctx, `
		INSERT INTO alert_deliveries (rule_id, project_id, issue_id, event_id, trigger, action, target, status, error)
		VALUES ($1, $2, $3::uuid, $4::uuid, $5, $6, $7, $8, $9)`,
		nullableUUID(ruleID), projectID, issueArg, eventArg, trigger, action, target, status, errMsg)
}

func nullableUUID(id string) any {
	if id == "" {
		return nil
	}
	return id
}

// SendWebhook POSTs a versioned payload with the BugHan headers: 5s timeout
// (client-enforced), X-BugHan-Event, and an HMAC signature when the rule
// carries a secret. Non-2xx is an error.
func SendWebhook(ctx context.Context, client *http.Client, url, secret, event string, body []byte) error {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	if event == "" {
		event = alerts.EventAlert
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(alerts.HeaderEvent, event)
	if secret != "" {
		req.Header.Set(alerts.HeaderSignature, alerts.Sign(secret, body))
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook %s: status %d", url, resp.StatusCode)
	}
	return nil
}
