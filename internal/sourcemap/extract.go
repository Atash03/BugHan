// Package sourcemap implements artifact matching and symbolication for
// JavaScript stack traces (DESIGN.md §10): sourcemap v3 parsing, VLQ
// decoding, (release, dist, abs_path) + debug_id artifact matching, and
// view-time symbolication with write-back caching.
package sourcemap

import (
	"encoding/json"
	"regexp"
	"strings"
)

// tailScanBytes bounds the artifact tail scanned for reference comments;
// bundlers append debugId/sourceMappingURL at the very end.
const tailScanBytes = 64 << 10

var (
	debugIDRe       = regexp.MustCompile(`(?i)[/*#@]\s*debugId[=:]\s*([0-9a-fA-F-]{36})`)
	sourceMappingRe = regexp.MustCompile(`(?i)[/*#@]\s*sourceMappingURL[=:]\s*(\S+)`)
)

// ExtractArtifact scans one uploaded artifact for the references symbolication
// matches on: its debug id (//# debugId=… in scripts, the "debugId" field in
// sourcemaps) and, for scripts, the raw sourceMappingURL value. Either may be
// empty — most artifacts carry none.
func ExtractArtifact(name string, content []byte) (debugID, sourcemapURL string) {
	if isSourcemapArtifact(name, content) {
		var m struct {
			DebugID string `json:"debugId"`
		}
		if err := json.Unmarshal(content, &m); err == nil {
			debugID = strings.ToLower(strings.TrimSpace(m.DebugID))
		}
		return debugID, ""
	}
	if len(content) > tailScanBytes {
		content = content[len(content)-tailScanBytes:]
	}
	tail := string(content)
	if m := debugIDRe.FindStringSubmatch(tail); m != nil {
		debugID = strings.ToLower(m[1])
	}
	if m := sourceMappingRe.FindStringSubmatch(tail); m != nil {
		sourcemapURL = m[1]
	}
	return debugID, sourcemapURL
}

// isSourcemapArtifact decides whether an upload is a sourcemap: by the .map
// extension, or by content shape (a JSON object with v3 markers) for
// extension-less uploads.
func isSourcemapArtifact(name string, content []byte) bool {
	if strings.HasSuffix(name, ".map") {
		return true
	}
	trimmed := strings.TrimLeftFunc(string(content), func(r rune) bool { return r == ' ' || r == '\n' || r == '\r' || r == '\t' })
	if !strings.HasPrefix(trimmed, "{") {
		return false
	}
	var probe struct {
		Version  int      `json:"version"`
		Sources  []string `json:"sources"`
		Mappings string   `json:"mappings"`
	}
	if err := json.Unmarshal(content, &probe); err != nil {
		return false
	}
	return probe.Version == 3 && probe.Mappings != "" || len(probe.Sources) > 0
}
