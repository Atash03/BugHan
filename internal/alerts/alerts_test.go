package alerts

import (
	"encoding/json"
	"testing"
)

func TestMatchesFilters(t *testing.T) {
	r := &Rule{Environments: []string{"production"}, Levels: []string{"error"}}
	if !r.MatchesFilters(Signal{Environment: "production", Level: "Error", Release: "1.0"}) {
		t.Fatal("expected match (case-insensitive level)")
	}
	if r.MatchesFilters(Signal{Environment: "staging", Level: "error"}) {
		t.Fatal("staging should not match production filter")
	}
	// Empty filters match everything.
	open := &Rule{}
	if !open.MatchesFilters(Signal{Environment: "x", Release: "y", Level: "z"}) {
		t.Fatal("empty filters must match")
	}
}

func TestMatchesTrigger(t *testing.T) {
	if !(&Rule{Trigger: "new_issue"}).MatchesTrigger(Signal{IsNew: true}, 0) {
		t.Fatal("new_issue should fire on IsNew")
	}
	if (&Rule{Trigger: "new_issue"}).MatchesTrigger(Signal{}, 0) {
		t.Fatal("new_issue must not fire on old issue")
	}
	if !(&Rule{Trigger: "regression"}).MatchesTrigger(Signal{IsRegression: true}, 0) {
		t.Fatal("regression should fire on IsRegression")
	}
	ec := &Rule{Trigger: "event_count", ThresholdCount: 5}
	if !ec.MatchesTrigger(Signal{}, 5) {
		t.Fatal("event_count should fire at threshold")
	}
	if ec.MatchesTrigger(Signal{}, 4) {
		t.Fatal("event_count must not fire below threshold")
	}
	if (&Rule{Trigger: "bogus"}).MatchesTrigger(Signal{IsNew: true}, 99) {
		t.Fatal("unknown trigger must never fire")
	}
}

func TestShouldSendRespectsEnabled(t *testing.T) {
	r := &Rule{Trigger: "new_issue", Enabled: false}
	if r.ShouldSend(Signal{IsNew: true}, 0) {
		t.Fatal("disabled rule must not send")
	}
}

func TestSignFormat(t *testing.T) {
	sig := Sign("secret", []byte(`{}`))
	if len(sig) != len("sha256=")+64 {
		t.Fatalf("bad signature shape %q", sig)
	}
	if sig[:7] != "sha256=" {
		t.Fatalf("missing prefix %q", sig)
	}
}

func TestBuildPayloadVersioned(t *testing.T) {
	p := BuildPayload(EventAlert,
		map[string]any{"id": "r1", "name": "N"},
		map[string]any{"id": "p1"},
		map[string]any{"id": "i1"},
		"new_issue",
		map[string]any{"issue": "http://x/i1"})
	raw, _ := json.Marshal(p)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	if decoded["version"] != float64(PayloadVersion) {
		t.Fatalf("payload must carry version %d", PayloadVersion)
	}
	if decoded["event"] != EventAlert {
		t.Fatalf("event field wrong: %v", decoded["event"])
	}
}

func TestNormalizeEmails(t *testing.T) {
	got := NormalizeEmails([]string{" A@x.com ", "a@x.com", "bad", "", "B@y.io"})
	if len(got) != 2 || got[0] != "a@x.com" || got[1] != "b@y.io" {
		t.Fatalf("unexpected normalization: %v", got)
	}
}
