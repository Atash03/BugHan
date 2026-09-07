package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/Atash03/BugHan/internal/auth"
	"github.com/Atash03/BugHan/internal/web"
	"github.com/Atash03/BugHan/internal/worker"
	"github.com/google/uuid"
)

// alertRuleCard is the settings-page row shape for one rule.
type alertRuleCard struct {
	ID, Name, Trigger          string
	Enabled                    bool
	Environments, Releases     []string
	Levels, EmailTo            []string
	ThresholdCount             int
	ThresholdMinutes           int
	QuietMinutes               int
	WebhookURL                 string
	HasSecret                  bool
}

// alertDeliveryCard is one delivery-log row.
type alertDeliveryCard struct {
	RuleName, Trigger, Action, Target, Status, Error, Created string
}

// uiAlertRules loads a project's rules for the settings page.
func (s *Server) uiAlertRules(r *http.Request, projectID string) []alertRuleCard {
	rows, err := s.pool.Query(r.Context(), `
		SELECT id::text, COALESCE(name, ''), trigger, enabled,
		       COALESCE(environments, '{}'), COALESCE(releases, '{}'), COALESCE(levels, '{}'),
		       threshold_count, threshold_minutes, quiet_minutes,
		       COALESCE(email_to, '{}'), COALESCE(webhook_url, ''), (webhook_secret <> '')
		FROM alert_rules WHERE project_id = $1 ORDER BY created_at`, projectID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []alertRuleCard{}
	for rows.Next() {
		var c alertRuleCard
		_ = rows.Scan(&c.ID, &c.Name, &c.Trigger, &c.Enabled,
			&c.Environments, &c.Releases, &c.Levels,
			&c.ThresholdCount, &c.ThresholdMinutes, &c.QuietMinutes,
			&c.EmailTo, &c.WebhookURL, &c.HasSecret)
		out = append(out, c)
	}
	return out
}

// uiAlertDeliveries loads the latest delivery-log rows for the settings page.
func (s *Server) uiAlertDeliveries(r *http.Request, projectID string) []alertDeliveryCard {
	rows, err := s.pool.Query(r.Context(), `
		SELECT COALESCE(ar.name, ''), d.trigger, d.action, d.target, d.status, d.error, d.created_at::text
		FROM alert_deliveries d
		LEFT JOIN alert_rules ar ON ar.id = d.rule_id
		WHERE d.project_id = $1
		ORDER BY d.created_at DESC, d.id DESC LIMIT 25`, projectID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []alertDeliveryCard{}
	for rows.Next() {
		var c alertDeliveryCard
		_ = rows.Scan(&c.RuleName, &c.Trigger, &c.Action, &c.Target, &c.Status, &c.Error, &c.Created)
		out = append(out, c)
	}
	return out
}

// splitCSV parses a comma-separated form field into trimmed non-empty parts.
func splitCSV(v string) []string {
	out := []string{}
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// uiCreateAlertRule handles the settings-page rule form (admin only).
func (s *Server) uiCreateAlertRule(w http.ResponseWriter, r *http.Request) {
	if s.uiUser(w, r) == nil {
		return
	}
	o, p := s.uiProject(r)
	if o == nil || !roleAtLeast(s.orgRole(r, o.Slug), "admin") {
		s.uiError(w, r, http.StatusForbidden, "Only admins can manage alert rules.", nil)
		return
	}
	back := "/" + o.Slug + "/" + p.Slug + "/settings/"
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, back, http.StatusFound)
		return
	}
	thresholdCount, _ := strconv.Atoi(strings.TrimSpace(r.PostFormValue("threshold_count")))
	thresholdMinutes, _ := strconv.Atoi(strings.TrimSpace(r.PostFormValue("threshold_minutes")))
	quietMinutes := 30
	if v := strings.TrimSpace(r.PostFormValue("quiet_minutes")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			quietMinutes = n
		}
	}
	in := ruleInput{
		Name:             strings.TrimSpace(r.PostFormValue("name")),
		Trigger:          strings.TrimSpace(r.PostFormValue("trigger")),
		Environments:     splitCSV(r.PostFormValue("environments")),
		Releases:         splitCSV(r.PostFormValue("releases")),
		Levels:           splitCSV(r.PostFormValue("levels")),
		ThresholdCount:   thresholdCount,
		ThresholdMinutes: thresholdMinutes,
		EmailTo:          splitCSV(r.PostFormValue("email_to")),
		WebhookURL:       strings.TrimSpace(r.PostFormValue("webhook_url")),
	}
	enabled := r.PostFormValue("enabled") != ""
	in.Enabled = &enabled
	in.QuietMinutes = &quietMinutes
	if v, msg := validateRuleInput(in); msg != "" {
		s.uiError(w, r, http.StatusBadRequest, "Invalid alert rule: "+msg, nil)
		return
	} else {
		secret := ""
		if v.webhookURL != "" {
			secret = newWebhookSecret()
		}
		actor := ""
		if me := auth.FromContext(r.Context()); me != nil {
			actor = me.ID
		}
		_, _ = s.pool.Exec(r.Context(), `
			INSERT INTO alert_rules (project_id, name, trigger, enabled,
				environments, releases, levels, threshold_count, threshold_minutes,
				quiet_minutes, email_to, webhook_url, webhook_secret, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13, NULLIF($14, '')::uuid)`,
			p.ID, v.name, v.trigger, v.enabled,
			v.environments, v.releases, v.levels, v.thresholdCount, v.thresholdMinutes,
			v.quietMinutes, v.emailTo, v.webhookURL, secret, actor)
	}
	http.Redirect(w, r, back, http.StatusFound)
}

// uiDeleteAlertRule removes a rule (admin only).
func (s *Server) uiDeleteAlertRule(w http.ResponseWriter, r *http.Request) {
	if s.uiUser(w, r) == nil {
		return
	}
	o, p := s.uiProject(r)
	if o == nil || !roleAtLeast(s.orgRole(r, o.Slug), "admin") {
		s.uiError(w, r, http.StatusForbidden, "Only admins can manage alert rules.", nil)
		return
	}
	if id, err := uuid.Parse(r.PathValue("ruleID")); err == nil {
		_, _ = s.pool.Exec(r.Context(),
			`DELETE FROM alert_rules WHERE id = $1 AND project_id = $2`, id, p.ID)
	}
	http.Redirect(w, r, "/"+o.Slug+"/"+p.Slug+"/settings/", http.StatusFound)
}

// uiToggleAlertRule flips a rule's enabled flag (admin only).
func (s *Server) uiToggleAlertRule(w http.ResponseWriter, r *http.Request) {
	if s.uiUser(w, r) == nil {
		return
	}
	o, p := s.uiProject(r)
	if o == nil || !roleAtLeast(s.orgRole(r, o.Slug), "admin") {
		s.uiError(w, r, http.StatusForbidden, "Only admins can manage alert rules.", nil)
		return
	}
	if id, err := uuid.Parse(r.PathValue("ruleID")); err == nil {
		_, _ = s.pool.Exec(r.Context(), `
			UPDATE alert_rules SET enabled = NOT enabled, updated_at = now()
			WHERE id = $1 AND project_id = $2`, id, p.ID)
	}
	http.Redirect(w, r, "/"+o.Slug+"/"+p.Slug+"/settings/", http.StatusFound)
}

// uiTestAlertRule fires a rule's actions with a test payload (admin only).
func (s *Server) uiTestAlertRule(w http.ResponseWriter, r *http.Request) {
	if s.uiUser(w, r) == nil {
		return
	}
	o, p := s.uiProject(r)
	if o == nil || !roleAtLeast(s.orgRole(r, o.Slug), "admin") {
		s.uiError(w, r, http.StatusForbidden, "Only admins can test alert rules.", nil)
		return
	}
	back := "/" + o.Slug + "/" + p.Slug + "/settings/"
	id, err := uuid.Parse(r.PathValue("ruleID"))
	if err != nil {
		http.Redirect(w, r, back, http.StatusFound)
		return
	}
	var ruleID string
	if err := s.pool.QueryRow(r.Context(), `
		SELECT id::text FROM alert_rules WHERE id = $1 AND project_id = $2`,
		id, p.ID).Scan(&ruleID); err != nil {
		http.Redirect(w, r, back, http.StatusFound)
		return
	}
	deps := worker.AlertsDeps{Cfg: s.cfg, Mailer: s.mailer, Log: s.log}
	sent, failed, _ := worker.SendTest(r.Context(), s.pool, deps, ruleID, p.ID)
	web.Render(w, 200, "app_alert_tested", web.PageData{
		Title: "Alert test sent",
		Data: map[string]any{
			"nav": s.uiNav(r, auth.FromContext(r.Context()), o, p, "settings"),
			"org": o, "project": p, "sent": sent, "failed": failed,
		},
	})
}
