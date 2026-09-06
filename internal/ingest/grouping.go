package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
)

// groupingVersion is the id of the grouping algorithm these functions
// implement; issues record it so future algorithm changes never reshuffle
// existing groups (DESIGN.md §8).
const groupingVersion = 1

// normalizedEvent holds the grouping_v1 fields derived from a raw error-event
// payload.
type normalizedEvent struct {
	Title       string // normalized "type: message"
	Culprit     string // normalized "path in function", "" when title-only
	Fingerprint string // group key: default hash or "f:" + SDK fingerprint
}

// sentryException is one entry of the exception.values array; the newest
// exception is the LAST element.
type sentryException struct {
	Type       string `json:"type"`
	Value      string `json:"value"`
	Stacktrace struct {
		Frames []sentryFrame `json:"frames"`
	} `json:"stacktrace"`
}

// sentryExceptionGroup is the `exception` interface: an object wrapping the
// values array, not an array itself.
type sentryExceptionGroup struct {
	Values []sentryException `json:"values"`
}

// sentryFrame mirrors the fields grouping reads. Frames are ordered
// root→leaf; the "top" frame is the LAST one.
type sentryFrame struct {
	Filename string `json:"filename"`
	AbsPath  string `json:"abs_path"`
	Function string `json:"function"`
	Module   string `json:"module"`
	InApp    *bool  `json:"in_app"`
}

func (f sentryFrame) inApp() bool { return f.InApp != nil && *f.InApp }

// normalizeEvent derives the grouping_v1 fields from a raw error-event payload.
func normalizeEvent(raw []byte) (normalizedEvent, error) {
	var payload struct {
		Message     string               `json:"message"`
		Exceptions  sentryExceptionGroup `json:"exception"`
		Fingerprint []string             `json:"fingerprint"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return normalizedEvent{}, err
	}

	title, culprit := "", ""
	if len(payload.Exceptions.Values) > 0 {
		exc := payload.Exceptions.Values[len(payload.Exceptions.Values)-1] // newest exception
		title = normalizeTitle(exc.Type, exc.Value)
		culprit = normalizedCulprit(exc.Stacktrace.Frames)
	} else {
		title = normalizeTitle("", payload.Message)
	}

	return normalizedEvent{
		Title:       title,
		Culprit:     culprit,
		Fingerprint: fingerprintV1(payload.Fingerprint, title, culprit),
	}, nil
}

// defaultTokenRe matches the {{ default }} token with or without inner
// whitespace, per Sentry semantics.
var defaultTokenRe = regexp.MustCompile(`\{\{\s*default\s*\}\}`)

// fingerprintV1 computes the group key: the SDK fingerprint array wins
// ({{ default }} tokens expanded to the computed hash, "f:" namespace), else
// SHA-256 over grouping_v1 + event type + title + culprit.
func fingerprintV1(sdkFP []string, title, culprit string) string {
	def := defaultHash(title, culprit)
	if len(sdkFP) == 0 {
		return def
	}
	parts := make([]string, len(sdkFP))
	for i, p := range sdkFP {
		p = strings.TrimSpace(p)
		if defaultTokenRe.MatchString(p) {
			p = def
		}
		parts[i] = p
	}
	return "f:" + strings.Join(parts, "\x1f")
}

// defaultHash = SHA-256(grouping_v1 \0 type \0 title \0 culprit); culprit is
// empty for title-only grouping.
func defaultHash(title, culprit string) string {
	h := sha256.New()
	h.Write([]byte("grouping_v1\x00error\x00"))
	h.Write([]byte(title))
	h.Write([]byte("\x00"))
	h.Write([]byte(culprit))
	return hex.EncodeToString(h.Sum(nil))
}

// normalizeTitle joins the exception type and value, parameterizes unstable
// content, and truncates to the 256-char grouping cap.
func normalizeTitle(typ, value string) string {
	if typ != "" {
		value = typ + ": " + value
	}
	return truncateGrouping(parameterize(value), 256)
}

// UUID shape first, then any hex run ≥8 chars (hashes, long ids), then the
// remaining short numbers. Interpolated %s/%d params are stable as-is.
var (
	uuidRe = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	hexRe  = regexp.MustCompile(`[0-9a-fA-F]{8,}`)
	numRe  = regexp.MustCompile(`[0-9]+`)
)

func parameterize(s string) string {
	s = uuidRe.ReplaceAllString(s, "{uuid}")
	s = hexRe.ReplaceAllString(s, "{hex}")
	return numRe.ReplaceAllString(s, "{num}")
}

// truncateGrouping caps the title at 256 bytes without the ellipsis the
// display truncate() adds — the hash input must stay byte-stable.
func truncateGrouping(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// normalizedCulprit picks the top in-app frame and renders "path in function".
// Anonymous/webpack frames are skipped in favor of the next in-app frame; no
// in-app frame → title-only grouping (empty culprit).
func normalizedCulprit(frames []sentryFrame) string {
	var chosen *sentryFrame
	for i := len(frames) - 1; i >= 0; i-- {
		f := &frames[i]
		if !f.inApp() {
			continue
		}
		if isAnonymousFrame(f) || isWebpackFrame(f) {
			continue
		}
		chosen = f
		break
	}
	if chosen == nil {
		return ""
	}
	path := chosen.Filename
	if path == "" {
		path = chosen.AbsPath
	}
	path = normalizeBundlePath(stripQueryFragment(path))
	if chosen.Function == "" {
		return path
	}
	return path + " in " + chosen.Function
}

// normalizeBundlePath replaces bundle-hash path segments with {hash} so a
// re-deploy doesn't split groups: dotted (main.9ab34f.js — webpack's default
// 6-char hash), dash (index-9ab34f12ab.js), and bare hex directory
// (a1b2c3d4e5f6/) styles.
var (
	dottedHashRe = regexp.MustCompile(`^(.+)\.([0-9a-fA-F]{6,})\.([a-zA-Z0-9]+)$`)
	dashHashRe   = regexp.MustCompile(`^(.+)-([0-9a-fA-F]{8,})(\.[a-zA-Z0-9]+)?$`)
)

func normalizeBundlePath(path string) string {
	segs := strings.Split(path, "/")
	for i, seg := range segs {
		if m := dottedHashRe.FindStringSubmatch(seg); m != nil {
			segs[i] = m[1] + ".{hash}." + m[3]
			continue
		}
		if m := dashHashRe.FindStringSubmatch(seg); m != nil {
			segs[i] = m[1] + "-{hash}" + m[3]
			continue
		}
		if len(seg) >= 8 && isAllHex(seg) {
			segs[i] = "{hash}"
		}
	}
	return strings.Join(segs, "/")
}

func isAllHex(s string) bool {
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return len(s) > 0
}

func isAnonymousFrame(f *sentryFrame) bool {
	switch f.Function {
	case "", "?", "<anonymous>", "anonymous":
		return true
	}
	return false
}

func isWebpackFrame(f *sentryFrame) bool {
	if strings.HasPrefix(f.Module, "webpack") {
		return true
	}
	if strings.Contains(f.Filename, "webpack") || strings.Contains(f.AbsPath, "webpack") {
		return true
	}
	return false
}

// stripQueryFragment removes ?query and #fragment from a path or URL.
func stripQueryFragment(s string) string {
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		return s[:i]
	}
	return s
}
