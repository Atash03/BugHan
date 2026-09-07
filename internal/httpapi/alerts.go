package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Atash03/BugHan/internal/alerts"
	"github.com/Atash03/BugHan/internal/worker"
	"github.com/google/uuid"
)

// registerAlerts wires the thin-alerting API (DESIGN.md §12, ticket T10):
// rule CRUD, send-test, and the delivery log. Reads need member; mutations
// need admin (project-settings surface per CONTEXT.md roles).
func (s *Server) registerAlerts(mux *http.ServeMux) {
	api := func(h http.HandlerFunc) http.Handler { return s.requireAuth(h) }
	mutate := func(h http.Handler) http.Handler {
		return s.requireCSRF(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if principalScope(r.Context()) == "read" {
				writeErr(w, http.StatusForbidden, "API token has read-only scope")
				return
			}
			h.ServeHTTP(w, r)
		}))
	}
	requireProject := func(h http.HandlerFunc, minRole string) http.Handler {
		return api(func(w http.ResponseWriter, r *http.Request) {
			p := s.projectFromPath(r)
			if p == nil {
				writeErr(w, http.StatusNotFound, "project not found")
				return
			}
			if !roleAtLeast(s.orgRole(r, p.OrgSlug), minRole) {
				writeErr(w, http.StatusForbidden, "insufficient role")
				return
			}
			h(w, r)
		})
	}

	mux.Handle("GET /api/0/projects/{org}/{project}/alerts/", requireProject(s.handleListRules, "member"))
	mux.Handle("POST /api/0/projects/{org}/{project}/alerts/", mutate(requireProject(s.handleCreateRule, "admin")))
	mux.Handle("GET /api/0/projects/{org}/{project}/alerts/{rule}/", requireProject(s.handleRuleDetail, "member"))
	mux.Handle("PUT /api/0/projects/{org}/{project}/alerts/{rule}/", mutate(requireProject(s.handleUpdateRule, "admin")))
	mux.Handle("DELETE /api/0/projects/{org}/{project}/alerts/{rule}/", mutate(requireProject(s.handleDeleteRule, "admin")))
	mux.Handle("POST /api/0/projects/{org}/{project}/alerts/{rule}/test/", mutate(requireProject(s.handleTestRule, "admin")))
	mux.Handle("GET /api/0/projects/{org}/{project}/alerts-deliveries/", requireProject(s.handleListDeliveries, "member"))
}

// ruleInput is the create/update body. PUT accepts the same shape; every
// field is applied (full replacement, like the issues PUT).
type ruleInput struct {
	Name             string   `json:"name"`
	Trigger          string   `json:"trigger"`
	Enabled          *bool    `json:"enabled"`
	Environments     []string `json:"environments"`
	Releases         []string `json:"releases"`
	Levels           []string `json:"levels"`
	ThresholdCount   int      `json:"threshold_count"`
	ThresholdMinutes int      `json:"threshold_minutes"`
	QuietMinutes     *int     `json:"quiet_minutes"`
	EmailTo          []string `json:"email_to"`
	WebhookURL       string   `json:"webhook_url"`
	RotateSecret     bool     `json:"rotate_secret"`
}

// validatedRule is the sanitized form ready for INSERT/UPDATE.
type validatedRule struct {
	name             string
	trigger          string
	enabled          bool
	environments     []string
	releases         []string
	levels           []string
	thresholdCount   int
	thresholdMinutes int
	quietMinutes     int
	emailTo          []string
	webhookURL       string
}

func validateRuleInput(in ruleInput) (*validatedRule, string) {
	out := &validatedRule{
		name:             strings.TrimSpace(in.Name),
		environments:     nonEmptyTrimmed(in.Environments),
		releases:         nonEmptyTrimmed(in.Releases),
		levels:           lowerTrimmed(in.Levels),
		thresholdCount:   in.ThresholdCount,
		thresholdMinutes: in.ThresholdMinutes,
		webhookURL:       strings.TrimSpace(in.WebhookURL),
	}
	out.enabled = true
	if in.Enabled != nil {
		out.enabled = *in.Enabled
	}
	out.quietMinutes = 30
	if in.QuietMinutes != nil {
		out.quietMinutes = *in.QuietMinutes
	}
	switch in.Trigger {
	case "new_issue", "regression", "event_count":
		out.trigger = in.Trigger
	default:
		return nil, "trigger must be new_issue, regression or event_count"
	}
	if out.trigger == "event_count" {
		if out.thresholdCount <= 0 {
			out.thresholdCount = 10
		}
		if out.thresholdMinutes <= 0 {
			out.thresholdMinutes = 60
		}
	}
	if out.quietMinutes < 0 {
		return nil, "quiet_minutes must be >= 0 (0 disables the quiet period)"
	}
	out.emailTo = alerts.NormalizeEmails(in.EmailTo)
	if out.webhookURL != "" {
		u, err := url.Parse(out.webhookURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, "webhook_url must be an http(s) URL"
		}
	}
	if len(out.emailTo) == 0 && out.webhookURL == "" {
		return nil, "rule needs at least one action: email_to or webhook_url"
	}
	return out, ""
}

func nonEmptyTrimmed(in []string) []string {
	out := []string{}
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func lowerTrimmed(in []string) []string {
	out := []string{}
	for _, v := range in {
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func newWebhookSecret() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// scanRuleRow maps one alert_rules row to its API shape. The webhook secret
// is never returned (rotate via rotate_secret); has_secret tells the UI
// whether signing is active.
func (s *Server) scanRuleRow(sc rowScanner) map[string]any {
	var id, projectID, name, trigger string
	var enabled bool
	var envs, rels, levels, emailTo []string
	var tCount, tMins, quiet int
	var webhookURL string
	var createdAt, updatedAt string
	if err := sc.Scan(&id, &projectID, &name, &trigger, &enabled,
		&envs, &rels, &levels, &tCount, &tMins, &quiet,
		&emailTo, &webhookURL, &createdAt, &updatedAt); err != nil {
		return map[string]any{"error": err.Error()}
	}
	return ruleJSON(id, projectID, name, trigger, enabled, envs, rels, levels,
		tCount, tMins, quiet, emailTo, webhookURL, createdAt, updatedAt)
}

func ruleJSON(id, projectID, name, trigger string, enabled bool, envs, rels, levels []string, tCount, tMins, quiet int, emailTo []string, webhookURL, createdAt, updatedAt string) map[string]any {
	return map[string]any{
		"id": id, "project_id": projectID, "name": name, "trigger": trigger,
		"enabled": enabled, "environments": envs, "releases": rels, "levels": levels,
		"threshold_count": tCount, "threshold_minutes": tMins, "quiet_minutes": quiet,
		"email_to": emailTo, "webhook_url": webhookURL,
		"created_at": createdAt, "updated_at": updatedAt,
	}
}

const ruleSelectCols = `id::text, project_id::text, COALESCE(name, ''), trigger, enabled,
	COALESCE(environments, '{}'), COALESCE(releases, '{}'), COALESCE(levels, '{}'),
	threshold_count, threshold_minutes, quiet_minutes,
	COALESCE(email_to, '{}'), COALESCE(webhook_url, ''),
	created_at::text, updated_at::text`

func (s *Server) handleListRules(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromPath(r)
	rows, err := s.pool.Query(r.Context(), `
		SELECT `+ruleSelectCols+` FROM alert_rules
		WHERE project_id = $1 ORDER BY created_at`, p.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	rules := []map[string]any{}
	for rows.Next() {
		rules = append(rules, s.scanRuleRow(rows))
	}
	writeJSON(w, rules)
}

func (s *Server) handleCreateRule(w http.ResponseWriter, r *http.Request) {
	var in ruleInput
	if !decodeBody(w, r, &in) {
		return
	}
	v, errMsg := validateRuleInput(in)
	if errMsg != "" {
		writeErr(w, http.StatusBadRequest, errMsg)
		return
	}
	p := s.projectFromPath(r)
	u := principalID(r)
	secret := ""
	if v.webhookURL != "" {
		secret = newWebhookSecret()
	}
	var id string
	err := s.pool.QueryRow(r.Context(), `
		INSERT INTO alert_rules (project_id, name, trigger, enabled,
			environments, releases, levels, threshold_count, threshold_minutes,
			quiet_minutes, email_to, webhook_url, webhook_secret, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13, NULLIF($14, '')::uuid)
		RETURNING id::text`,
		p.ID, v.name, v.trigger, v.enabled,
		v.environments, v.releases, v.levels, v.thresholdCount, v.thresholdMinutes,
		v.quietMinutes, v.emailTo, v.webhookURL, secret, u).Scan(&id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.writeRule(w, r, p.ID, id, http.StatusCreated)
}

func (s *Server) ruleFromPath(r *http.Request, projectID string) string {
	id := r.PathValue("rule")
	if _, err := uuid.Parse(id); err != nil {
		return ""
	}
	var found string
	if err := s.pool.QueryRow(r.Context(), `
		SELECT id::text FROM alert_rules WHERE id = $1 AND project_id = $2`,
		id, projectID).Scan(&found); err != nil {
		return ""
	}
	return found
}

func (s *Server) writeRule(w http.ResponseWriter, r *http.Request, projectID, ruleID string, status int) {
	row := s.pool.QueryRow(r.Context(), `
		SELECT `+ruleSelectCols+` FROM alert_rules WHERE id = $1 AND project_id = $2`,
		ruleID, projectID)
	writeJSONStatus(w, status, s.scanRuleRow(row))
}

func (s *Server) handleRuleDetail(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromPath(r)
	id := s.ruleFromPath(r, p.ID)
	if id == "" {
		writeErr(w, http.StatusNotFound, "rule not found")
		return
	}
	s.writeRule(w, r, p.ID, id, http.StatusOK)
}

func (s *Server) handleUpdateRule(w http.ResponseWriter, r *http.Request) {
	var in ruleInput
	if !decodeBody(w, r, &in) {
		return
	}
	v, errMsg := validateRuleInput(in)
	if errMsg != "" {
		writeErr(w, http.StatusBadRequest, errMsg)
		return
	}
	p := s.projectFromPath(r)
	id := s.ruleFromPath(r, p.ID)
	if id == "" {
		writeErr(w, http.StatusNotFound, "rule not found")
		return
	}
	// Keep the existing secret unless the URL is new/rotated; clear it when
	// the webhook is removed.
	var fresh string
	if v.webhookURL != "" && in.RotateSecret {
		fresh = newWebhookSecret()
	}
	ct, err := s.pool.Exec(r.Context(), `
		UPDATE alert_rules SET name = $3, trigger = $4, enabled = $5,
			environments = $6, releases = $7, levels = $8,
			threshold_count = $9, threshold_minutes = $10, quiet_minutes = $11,
			email_to = $12, webhook_url = $13,
			webhook_secret = CASE
				WHEN $13 = '' THEN ''
				WHEN $14 <> '' THEN $14
				WHEN webhook_secret = '' THEN $15
				ELSE webhook_secret END,
			updated_at = now()
		WHERE id = $1 AND project_id = $2`,
		id, p.ID, v.name, v.trigger, v.enabled,
		v.environments, v.releases, v.levels,
		v.thresholdCount, v.thresholdMinutes, v.quietMinutes,
		v.emailTo, v.webhookURL, fresh, newWebhookSecret())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if ct.RowsAffected() == 0 {
		writeErr(w, http.StatusNotFound, "rule not found")
		return
	}
	s.writeRule(w, r, p.ID, id, http.StatusOK)
}

func (s *Server) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromPath(r)
	id := s.ruleFromPath(r, p.ID)
	if id == "" {
		writeErr(w, http.StatusNotFound, "rule not found")
		return
	}
	_, _ = s.pool.Exec(r.Context(), `DELETE FROM alert_rules WHERE id = $1`, id)
	w.WriteHeader(http.StatusNoContent)
}

// handleTestRule fires the rule's actions immediately with a test payload
// (latest issue/event when one exists, synthetic otherwise) and records the
// attempts with action='test' in the delivery log.
func (s *Server) handleTestRule(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromPath(r)
	id := s.ruleFromPath(r, p.ID)
	if id == "" {
		writeErr(w, http.StatusNotFound, "rule not found")
		return
	}
	deps := worker.AlertsDeps{Cfg: s.cfg, Mailer: s.mailer, Log: s.log}
	sent, failed, err := worker.SendTest(r.Context(), s.pool, deps, id, p.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"sent": sent, "failed": failed})
}

// handleListDeliveries returns the project's delivery log, newest first,
// optionally narrowed to one rule or issue.
func (s *Server) handleListDeliveries(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromPath(r)
	limit, offset := pageParams(r)
	q := r.URL.Query()

	args := []any{p.ID}
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	conds := []string{"d.project_id = $1"}
	if rid := strings.TrimSpace(q.Get("rule_id")); rid != "" {
		if _, err := uuid.Parse(rid); err != nil {
			writeErr(w, http.StatusBadRequest, "rule_id must be a uuid")
			return
		}
		conds = append(conds, "d.rule_id = "+arg(rid))
	}
	if iid := strings.TrimSpace(q.Get("issue_id")); iid != "" {
		if _, err := uuid.Parse(iid); err != nil {
			writeErr(w, http.StatusBadRequest, "issue_id must be a uuid")
			return
		}
		conds = append(conds, "d.issue_id = "+arg(iid))
	}
	where := "WHERE " + strings.Join(conds, " AND ")

	var total int
	if err := s.pool.QueryRow(r.Context(),
		`SELECT count(*) FROM alert_deliveries d `+where, args...).Scan(&total); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	rows, err := s.pool.Query(r.Context(), `
		SELECT d.id::text, COALESCE(d.rule_id::text, ''), COALESCE(r.name, ''),
		       COALESCE(d.issue_id::text, ''), COALESCE(d.event_id::text, ''),
		       d.trigger, d.action, d.target,
		       d.status, d.error, d.created_at::text
		FROM alert_deliveries d
		LEFT JOIN alert_rules r ON r.id = d.rule_id
		`+where+`
		ORDER BY d.created_at DESC, d.id DESC
		LIMIT $`+strconv.Itoa(len(args)+1)+` OFFSET $`+strconv.Itoa(len(args)+2),
		append(args, limit, offset)...)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	data := []map[string]any{}
	for rows.Next() {
		var id, ruleID, ruleName, issueID, eventID, trigger, action, target, status, errMsg, created string
		if err := rows.Scan(&id, &ruleID, &ruleName, &issueID, &eventID,
			&trigger, &action, &target, &status, &errMsg, &created); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		// NULL rule/issue/event ids scan as '' via ::text on non-null? Guard:
		// LEFT JOIN miss → rule_id may be NULL; COALESCE above keeps name.
		data = append(data, map[string]any{
			"id": id, "rule_id": nullIfEmpty(ruleID), "rule_name": ruleName,
			"issue_id": nullIfEmpty(issueID), "event_id": nullIfEmpty(eventID),
			"trigger": trigger, "action": action, "target": target,
			"status": status, "error": errMsg, "created_at": created,
		})
	}
	writeJSON(w, map[string]any{
		"data": data,
		"meta": map[string]int{"total": total, "limit": limit, "offset": offset},
	})
}

func nullIfEmpty(v string) any {
	if v == "" {
		return nil
	}
	return v
}
