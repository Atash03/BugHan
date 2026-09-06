package ingest

import (
	"strconv"
	"strings"
	"testing"
)

// grouping_v1: two error events with the same exception type, message, and
// top in-app frame must land in the same group (DESIGN.md §8).
func TestFingerprintSameExceptionSameGroup(t *testing.T) {
	payload := `{"exception":{"values":[{"type":"TypeError","value":"Cannot read properties of undefined (reading 'id')","stacktrace":{"frames":[{"filename":"/src/app.js","function":"onClick","in_app":true},{"filename":"/vendor/lib.js","function":"dispatch","in_app":false}]}}]}}`
	a, err := normalizeEvent([]byte(payload))
	if err != nil {
		t.Fatalf("normalizeEvent: %v", err)
	}
	b, err := normalizeEvent([]byte(payload))
	if err != nil {
		t.Fatalf("normalizeEvent: %v", err)
	}
	if a.Fingerprint == "" {
		t.Fatal("fingerprint is empty")
	}
	if a.Fingerprint != b.Fingerprint {
		t.Fatalf("identical events grouped apart: %q vs %q", a.Fingerprint, b.Fingerprint)
	}
}

// Varying numbers in the message must not split groups: they normalize to a
// placeholder (DESIGN.md §8 "numbers/UUIDs/hex ≥8 chars normalized").
func TestFingerprintNormalizesNumbersInMessage(t *testing.T) {
	mk := func(n int) []byte {
		return []byte(`{"exception":{"values":[{"type":"RangeError","value":"Order ` + itoa(n) + ` is out of stock"}]}}`)
	}
	a, err := normalizeEvent(mk(1234))
	if err != nil {
		t.Fatalf("normalizeEvent: %v", err)
	}
	b, err := normalizeEvent(mk(5678))
	if err != nil {
		t.Fatalf("normalizeEvent: %v", err)
	}
	if a.Fingerprint != b.Fingerprint {
		t.Fatalf("number-varying messages grouped apart:\n  %s\n  %s", a.Title, b.Title)
	}
	if a.Title != "RangeError: Order {num} is out of stock" {
		t.Fatalf("title not parameterized: %q", a.Title)
	}
}

func itoa(n int) string { return strings.TrimSpace(strconv.Itoa(n)) }

// Culprit comes from the top in-app frame with query/fragment stripped and
// bundle-hash path segments replaced (DESIGN.md §8).
func TestCulpritNormalizesBundleHashAndQuery(t *testing.T) {
	cases := []struct {
		name, filename, want string
	}{
		{"dotted hash", "https://cdn.example.com/assets/main.9ab34f.js?v=2", "https://cdn.example.com/assets/main.{hash}.js"},
		{"dash hash", "https://cdn.example.com/assets/index-9ab34f12ab.js", "https://cdn.example.com/assets/index-{hash}.js"},
		{"hash directory", "https://cdn.example.com/a1b2c3d4e5f6/app.js#frag", "https://cdn.example.com/{hash}/app.js"},
		{"no hash", "https://cdn.example.com/static/app.js", "https://cdn.example.com/static/app.js"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := `{"exception":{"values":[{"type":"Error","value":"boom","stacktrace":{"frames":[{"filename":"` + tc.filename + `","function":"handleClick","in_app":true}]}}]}}`
			norm, err := normalizeEvent([]byte(payload))
			if err != nil {
				t.Fatalf("normalizeEvent: %v", err)
			}
			if norm.Culprit != tc.want+" in handleClick" {
				t.Fatalf("culprit = %q, want %q", norm.Culprit, tc.want+" in handleClick")
			}
		})
	}
}

// The top in-app frame is the INNERMOST one (last in_app frame in the
// root→leaf ordering).
func TestCulpritPicksInnermostInAppFrame(t *testing.T) {
	payload := `{"exception":{"values":[{"type":"Error","value":"x","stacktrace":{"frames":[
		{"filename":"/src/boot.js","function":"boot","in_app":true},
		{"filename":"/vendor/lib.js","function":"dispatch","in_app":false},
		{"filename":"/src/feature.js","function":"submit","in_app":true}]}}]}}`
	norm, err := normalizeEvent([]byte(payload))
	if err != nil {
		t.Fatalf("normalizeEvent: %v", err)
	}
	if norm.Culprit != "/src/feature.js in submit" {
		t.Fatalf("culprit = %q, want /src/feature.js in submit", norm.Culprit)
	}
}

// Anonymous and webpack frames don't anchor a group: fall through to the next
// in-app frame (DESIGN.md §8).
func TestCulpritSkipsAnonymousAndWebpackFrames(t *testing.T) {
	payload := `{"exception":{"values":[{"type":"Error","value":"x","stacktrace":{"frames":[
		{"filename":"/src/real.js","function":"onClick","in_app":true},
		{"filename":"webpack/bootstrap","function":"callCallback","in_app":true},
		{"filename":"/src/app.js","function":"<anonymous>","in_app":true}]}}]}}`
	norm, err := normalizeEvent([]byte(payload))
	if err != nil {
		t.Fatalf("normalizeEvent: %v", err)
	}
	if norm.Culprit != "/src/real.js in onClick" {
		t.Fatalf("culprit = %q, want /src/real.js in onClick", norm.Culprit)
	}
}

// No in-app frame → title-only grouping: culprit contributes nothing.
func TestNoInAppFrameTitleOnly(t *testing.T) {
	a, err := normalizeEvent([]byte(`{"exception":{"values":[{"type":"Error","value":"boom","stacktrace":{"frames":[
		{"filename":"/vendor/lib.js","function":"dispatch","in_app":false}]}}]}}`))
	if err != nil {
		t.Fatalf("normalizeEvent: %v", err)
	}
	b, err := normalizeEvent([]byte(`{"exception":{"values":[{"type":"Error","value":"boom","stacktrace":{"frames":[
		{"filename":"/other/vendor.js","function":"handle","in_app":false}]}}]}}`))
	if err != nil {
		t.Fatalf("normalizeEvent: %v", err)
	}
	if a.Culprit != "" || b.Culprit != "" {
		t.Fatalf("expected empty culprits, got %q and %q", a.Culprit, b.Culprit)
	}
	if a.Fingerprint != b.Fingerprint {
		t.Fatalf("title-only grouping failed: %q vs %q", a.Fingerprint, b.Fingerprint)
	}
}

// The SDK fingerprint array wins over the computed hash (DESIGN.md §8).
func TestSDKFingerprintWins(t *testing.T) {
	sdkEvent := `{"fingerprint":["checkout-flow"],"exception":{"values":[{"type":"Error","value":"a"}]}}`
	norm, err := normalizeEvent([]byte(sdkEvent))
	if err != nil {
		t.Fatalf("normalizeEvent: %v", err)
	}
	if norm.Fingerprint != "f:checkout-flow" {
		t.Fatalf("fingerprint = %q, want f:checkout-flow", norm.Fingerprint)
	}
	// Even when everything else differs, the SDK fingerprint pins the group.
	other, err := normalizeEvent([]byte(`{"fingerprint":["checkout-flow"],"exception":{"values":[{"type":"TypeError","value":"b"}]}}`))
	if err != nil {
		t.Fatalf("normalizeEvent: %v", err)
	}
	if other.Fingerprint != norm.Fingerprint {
		t.Fatalf("SDK fingerprint not honored: %q vs %q", other.Fingerprint, norm.Fingerprint)
	}
	// f: namespace never collides with default grouping.
	def, err := normalizeEvent([]byte(`{"exception":{"values":[{"type":"Error","value":"a"}]}}`))
	if err != nil {
		t.Fatalf("normalizeEvent: %v", err)
	}
	if def.Fingerprint == norm.Fingerprint {
		t.Fatal("SDK fingerprint collided with default namespace")
	}
}

// {{ default }} in an SDK fingerprint expands to the server-computed hash, so
// SDK fingerprint + automatic grouping can coexist (DESIGN.md §8).
func TestSDKFingerprintDefaultExpansion(t *testing.T) {
	base, err := normalizeEvent([]byte(`{"exception":{"values":[{"type":"Error","value":"boom"}]}}`))
	if err != nil {
		t.Fatalf("normalizeEvent: %v", err)
	}
	expanded, err := normalizeEvent([]byte(`{"fingerprint":["{{ default }}","extra"],"exception":{"values":[{"type":"Error","value":"boom"}]}}`))
	if err != nil {
		t.Fatalf("normalizeEvent: %v", err)
	}
	// Expanded key = default hash + remaining tokens, joined with \x1f.
	want := "f:" + base.Fingerprint + "\x1fextra"
	if expanded.Fingerprint != want {
		t.Fatalf("fingerprint = %q, want %q", expanded.Fingerprint, want)
	}
	// Same expansion on a different title must differ (default part tracks).
	other, err := normalizeEvent([]byte(`{"fingerprint":["{{ default }}","extra"],"exception":{"values":[{"type":"Error","value":"other"}]}}`))
	if err != nil {
		t.Fatalf("normalizeEvent: %v", err)
	}
	if other.Fingerprint == expanded.Fingerprint {
		t.Fatal("{{ default }} expansion lost title sensitivity")
	}
}

// Chained exceptions group by the LAST (newest) exception value.
func TestChainedExceptionsUseNewest(t *testing.T) {
	chained := `{"exception":{"values":[
		{"type":"TypeError","value":"inner cause","stacktrace":{"frames":[{"filename":"/src/a.js","function":"inner","in_app":true}]}},
		{"type":"Error","value":"outer wrap","stacktrace":{"frames":[{"filename":"/src/b.js","function":"outer","in_app":true}]}}]}}`
	norm, err := normalizeEvent([]byte(chained))
	if err != nil {
		t.Fatalf("normalizeEvent: %v", err)
	}
	if norm.Title != "Error: outer wrap" {
		t.Fatalf("title = %q, want the newest exception's title", norm.Title)
	}
	if norm.Culprit != "/src/b.js in outer" {
		t.Fatalf("culprit = %q, want the newest exception's culprit", norm.Culprit)
	}
}

// Message-only events (captureMessage) group by the normalized message.
func TestMessageOnlyEventGroupsByMessage(t *testing.T) {
	a, err := normalizeEvent([]byte(`{"message":"Order 12 failed"}`))
	if err != nil {
		t.Fatalf("normalizeEvent: %v", err)
	}
	b, err := normalizeEvent([]byte(`{"message":"Order 34 failed"}`))
	if err != nil {
		t.Fatalf("normalizeEvent: %v", err)
	}
	if a.Title != "Order {num} failed" {
		t.Fatalf("title = %q, want parameterized", a.Title)
	}
	if a.Fingerprint != b.Fingerprint {
		t.Fatalf("message-only events grouped apart: %q vs %q", a.Fingerprint, b.Fingerprint)
	}
}
