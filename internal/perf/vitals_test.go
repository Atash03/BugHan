package perf

import (
	"encoding/json"
	"testing"
)

// Web vitals surface on the trace view (DESIGN.md §11): extracted from a
// transaction's measurements map, rated against good/needs-improvement/poor
// thresholds. Tests pin the SDK wire shape and the rating boundaries.

const vitalsMeasurements = `{
	"lcp":  {"value": 1200, "unit": "millisecond"},
	"cls":  {"value": 0.05, "unit": ""},
	"inp":  {"value": 300,  "unit": "millisecond"},
	"fcp":  {"value": 900,  "unit": "millisecond"},
	"fp":   {"value": 700,  "unit": "millisecond"},
	"ttfb": {"value": 100,  "unit": "millisecond"},
	"broken": {"value": 5, "unit": "millisecond"}
}`

func TestExtractWebVitalsPullsKnownVitalsOnly(t *testing.T) {
	vitals := ExtractWebVitals(json.RawMessage(vitalsMeasurements))
	want := map[string]float64{"lcp": 1200, "cls": 0.05, "inp": 300, "fcp": 900, "fp": 700, "ttfb": 100}
	if len(vitals) != len(want) {
		t.Fatalf("got %d vitals %v, want 6: %v", len(vitals), vitals, want)
	}
	for k, v := range want {
		if vitals[k] != v {
			t.Fatalf("vital %s = %v, want %v", k, vitals[k], v)
		}
	}
}

func TestExtractWebVitalsToleratesJunk(t *testing.T) {
	for _, junk := range []json.RawMessage{nil, []byte(`{}`), []byte(`{"lcp":"fast"}`), []byte(`not json`)} {
		if v := ExtractWebVitals(junk); len(v) != 0 {
			t.Fatalf("ExtractWebVitals(%s) = %v, want empty", junk, v)
		}
	}
}

func TestVitalRatingBoundaries(t *testing.T) {
	cases := []struct {
		vital string
		value float64
		want  string
	}{
		{"lcp", 2500, "good"}, {"lcp", 2500.01, "meh"}, {"lcp", 4000, "meh"}, {"lcp", 4000.01, "poor"},
		{"cls", 0.1, "good"}, {"cls", 0.25, "meh"}, {"cls", 0.3, "poor"},
		{"ttfb", 800, "good"}, {"ttfb", 1800, "meh"}, {"ttfb", 2000, "poor"},
		{"inp", 200, "good"}, {"inp", 500, "meh"}, {"inp", 600, "poor"},
		{"fcp", 1000, "good"}, {"fcp", 3000, "meh"},
		{"fp", 1000, "good"}, {"fp", 3000, "meh"},
		{"nope", 100, ""}, // unknown vital has no rating
	}
	for _, c := range cases {
		if got := VitalRating(c.vital, c.value); got != c.want {
			t.Fatalf("VitalRating(%s, %v) = %q, want %q", c.vital, c.value, got, c.want)
		}
	}
}
