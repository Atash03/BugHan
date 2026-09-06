// Package perf holds the performance plane (DESIGN.md §11): web-vital
// extraction, trace and transaction queries, and release health rates.
package perf

import (
	"encoding/json"
)

// KnownVitals are the web vitals surfaced on the trace view, in display order.
var KnownVitals = []string{"lcp", "cls", "inp", "fcp", "fp", "ttfb"}

// ExtractWebVitals pulls the six known vitals out of a transaction's
// measurements map ({name: {value, unit}}); unknown keys and non-numeric
// values are dropped, and anything unparseable yields an empty map.
func ExtractWebVitals(measurements json.RawMessage) map[string]float64 {
	out := map[string]float64{}
	if len(measurements) == 0 {
		return out
	}
	var m map[string]struct {
		Value float64 `json:"value"`
		Unit  string  `json:"unit"`
	}
	if err := json.Unmarshal(measurements, &m); err != nil {
		return out
	}
	known := map[string]bool{}
	for _, k := range KnownVitals {
		known[k] = true
	}
	for k, mv := range m {
		if known[k] {
			out[k] = mv.Value
		}
	}
	return out
}

// vitalThresholds maps each vital to its [good, needs-improvement] ceilings
// (Google Core Web Vitals boundaries; values in milliseconds, CLS unitless).
var vitalThresholds = map[string][2]float64{
	"lcp":  {2500, 4000},
	"cls":  {0.1, 0.25},
	"inp":  {200, 500},
	"fcp":  {1000, 3000},
	"fp":   {1000, 3000},
	"ttfb": {800, 1800},
}

// VitalRating classifies a vital value as "good", "meh", or "poor";
// unknown vitals have no rating ("").
func VitalRating(vital string, value float64) string {
	t, ok := vitalThresholds[vital]
	if !ok {
		return ""
	}
	switch {
	case value <= t[0]:
		return "good"
	case value <= t[1]:
		return "meh"
	default:
		return "poor"
	}
}
