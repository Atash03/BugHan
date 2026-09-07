// Package alerts holds the thin-alerting domain logic (DESIGN.md §12):
// rule/filter matching, versioned webhook payloads, and HMAC signing.
// DB access and delivery live in the worker and HTTP layers; this package
// stays dependency-light so its behavior is unit-testable without Postgres.
package alerts

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// PayloadVersion is the webhook/email JSON contract version.
const PayloadVersion = 1

// Webhook headers (DESIGN.md §12).
const (
	HeaderEvent     = "X-BugHan-Event"
	HeaderSignature = "X-BugHan-Signature"
	EventAlert      = "alert"
	EventTest       = "alert-test"
)

// Rule mirrors alert_rules (arrays decoded as slices).
type Rule struct {
	ID               string
	ProjectID        string
	Name             string
	Trigger          string // new_issue | regression | event_count
	Enabled          bool
	Environments     []string
	Releases         []string
	Levels           []string
	ThresholdCount   int
	ThresholdMinutes int
	QuietMinutes     int
	EmailTo          []string
	WebhookURL       string
	WebhookSecret    string
}

// Signal is one stored error event, after the issue upsert.
type Signal struct {
	ProjectID    string
	IssueID      string
	EventID      string
	IsNew        bool // issue row was created by this event
	IsRegression bool // issue flipped resolved → regressed on this event
	Environment  string
	Release      string
	Level        string
}

// MatchesFilters reports whether the signal passes the rule's env/release/
// level scoping. Empty filter lists match everything.
func (r *Rule) MatchesFilters(s Signal) bool {
	if len(r.Environments) > 0 && !containsFold(r.Environments, s.Environment) {
		return false
	}
	if len(r.Releases) > 0 && !containsFold(r.Releases, s.Release) {
		return false
	}
	if len(r.Levels) > 0 && !containsFold(r.Levels, s.Level) {
		return false
	}
	return true
}

// MatchesTrigger reports whether the signal fires the rule's trigger.
// eventCountWindow is the issue's event count inside the rule's window
// (only consulted for event_count).
func (r *Rule) MatchesTrigger(s Signal, eventCountWindow int64) bool {
	switch r.Trigger {
	case "new_issue":
		return s.IsNew
	case "regression":
		return s.IsRegression
	case "event_count":
		n := r.ThresholdCount
		if n <= 0 {
			n = 1
		}
		return eventCountWindow >= int64(n)
	}
	return false
}

// ShouldSend combines enabled + filters + trigger. Dedupe (quiet period)
// is enforced by the worker against alert_dedupe, not here.
func (r *Rule) ShouldSend(s Signal, eventCountWindow int64) bool {
	if !r.Enabled {
		return false
	}
	return r.MatchesFilters(s) && r.MatchesTrigger(s, eventCountWindow)
}

func containsFold(list []string, v string) bool {
	for _, e := range list {
		if strings.EqualFold(e, v) {
			return true
		}
	}
	return false
}

// Payload is the versioned JSON contract delivered to webhooks and
// summarized in alert emails.
type Payload struct {
	Version int            `json:"version"`
	Event   string         `json:"event"` // "alert" | "alert-test"
	Rule    map[string]any `json:"rule"`
	Project map[string]any `json:"project"`
	Issue   map[string]any `json:"issue"`
	Trigger string         `json:"trigger"`
	URLs    map[string]any `json:"urls"`
}

// BuildPayload assembles the versioned payload. Callers pass already-shaped
// rule/project/issue maps so this package never touches the DB.
func BuildPayload(event string, rule map[string]any, project map[string]any, issue map[string]any, trigger string, urls map[string]any) Payload {
	if urls == nil {
		urls = map[string]any{}
	}
	return Payload{
		Version: PayloadVersion,
		Event:   event,
		Rule:    rule,
		Project: project,
		Issue:   issue,
		Trigger: trigger,
		URLs:    urls,
	}
}

// Sign returns the X-BugHan-Signature header value for a webhook body.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// NormalizeEmails trims, lowercases, and dedupes an address list.
func NormalizeEmails(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, e := range in {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" || seen[e] {
			continue
		}
		if !strings.Contains(e, "@") {
			continue
		}
		seen[e] = true
		out = append(out, e)
	}
	return out
}
