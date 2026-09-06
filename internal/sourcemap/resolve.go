package sourcemap

import (
	"encoding/base64"
	"net/url"
	"strings"
)

// Artifact is one release file as symbolication sees it.
type Artifact struct {
	Name         string // uploaded artifact name (path or URL)
	Dist         string
	DebugID      string
	SourcemapURL string // raw sourceMappingURL extracted from scripts
	Body         []byte // needed for inline data: maps; may be nil
}

// Index is the artifact set of one (project, release) for symbolication.
// Dist is the event's dist; it selects which artifacts are visible.
type Index struct {
	Artifacts []Artifact
	Dist      string
}

// candidates orders the artifacts visible to this event: exact-dist uploads
// first, then bare uploads. A bare event sees bare uploads only, and a
// dist-tagged artifact never serves a different dist.
func (ix *Index) candidates() []Artifact {
	var exact, bare []Artifact
	for _, a := range ix.Artifacts {
		switch {
		case a.Dist == "":
			bare = append(bare, a)
		case ix.Dist != "" && a.Dist == ix.Dist:
			exact = append(exact, a)
		}
	}
	return append(exact, bare...)
}

// ResolveMap finds the sourcemap for one frame's abs_path (DESIGN.md §10):
// debug_meta.images debug ids first, then the minified artifact's own
// debug_id, then its sourceMappingURL (relative or inline data:), then the
// abs_path + ".map" fallback. nil, nil means "no map found" — the caller
// renders a missing-map hint.
func (ix *Index) ResolveMap(absPath string, imageDebugIDs []string) (*Artifact, *Map) {
	p := normalizePath(absPath)
	cands := ix.candidates()
	byName := map[string]*Artifact{}
	byDebug := map[string]*Artifact{}
	for i := range cands {
		a := &cands[i]
		byName[a.Name] = a
		byName[normalizePath(a.Name)] = a
		if a.DebugID != "" {
			byDebug[a.DebugID] = a
		}
	}
	// findArt matches a path, then its bare filename (DESIGN.md §10 matches
	// on abs_path *or* filename — uploads may use either form).
	findArt := func(path string) *Artifact {
		if a := byName[path]; a != nil {
			return a
		}
		return byName[pathBase(path)]
	}

	// 1. debug_meta.images: debug ids of images whose code_file is this frame.
	for _, id := range imageDebugIDs {
		if a := byDebug[strings.ToLower(id)]; a != nil {
			if m := parseMapArtifact(a); m != nil {
				return a, m
			}
		}
	}

	// 2. The minified artifact itself.
	if art := findArt(p); art != nil {
		if art.DebugID != "" {
			if a := byDebug[art.DebugID]; a != nil {
				if m := parseMapArtifact(a); m != nil {
					return a, m
				}
			}
		}
		if art.SourcemapURL != "" {
			if a, m := ix.bySourcemapURL(art, p, findArt); m != nil {
				return a, m
			}
		}
	}

	// 3. abs_path + ".map" fallback.
	if a := findArt(p + ".map"); a != nil {
		if m := parseMapArtifact(a); m != nil {
			return a, m
		}
	}
	return nil, nil
}

// bySourcemapURL resolves one artifact's sourceMappingURL: inline data: URIs
// parse straight from the artifact body; everything else is URL-joined
// against the frame path and looked up by name.
func (ix *Index) bySourcemapURL(art *Artifact, framePath string, findArt func(string) *Artifact) (*Artifact, *Map) {
	if strings.HasPrefix(strings.ToLower(art.SourcemapURL), "data:") {
		if m := parseInlineMap(art.SourcemapURL); m != nil {
			return art, m
		}
		return nil, nil
	}
	base := framePath
	if base == "" {
		base = normalizePath(art.Name)
	}
	resolved := joinReference(base, art.SourcemapURL)
	if a := findArt(resolved); a != nil {
		if m := parseMapArtifact(a); m != nil {
			return a, m
		}
	}
	return nil, nil
}

// pathBase is the final path segment of a normalized path or URL.
func pathBase(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// joinReference resolves a sourceMappingURL against the minified file's path
// or URL, handling relative segments.
func joinReference(base, ref string) string {
	if strings.HasPrefix(base, "http://") || strings.HasPrefix(base, "https://") {
		if u, err := url.Parse(base); err == nil {
			if r, err := url.Parse(ref); err == nil {
				return u.ResolveReference(r).String()
			}
		}
	}
	i := strings.LastIndex(base, "/")
	dir := ""
	if i >= 0 {
		dir = base[:i]
	}
	return normalizePathJoin(dir, ref)
}

// normalizePathJoin joins a directory and a relative reference, cleaning
// ./ and ../ segments.
func normalizePathJoin(dir, ref string) string {
	if strings.HasPrefix(ref, "/") {
		return cleanPath(ref)
	}
	if dir == "" {
		return cleanPath(ref)
	}
	return cleanPath(dir + "/" + ref)
}

func cleanPath(p string) string {
	segs := strings.Split(p, "/")
	out := make([]string, 0, len(segs))
	for _, s := range segs {
		switch s {
		case "", ".":
			continue
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, s)
		}
	}
	// Preserve the leading slash (URL path), drop trailing emptiness.
	joined := strings.Join(out, "/")
	if strings.HasPrefix(p, "/") {
		return "/" + joined
	}
	return joined
}

// normalizePath strips scheme://host, query and fragment from a frame
// abs_path or artifact name so both sides of the match compare equal.
func normalizePath(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, "://"); i >= 0 {
		if rest := s[i+3:]; strings.Contains(rest, "/") {
			s = rest[strings.Index(rest, "/"):]
		}
	}
	return s
}

// parseMapArtifact parses a map artifact's body; nil when it isn't a map.
func parseMapArtifact(a *Artifact) *Map {
	if a == nil || len(a.Body) == 0 {
		return nil
	}
	m, err := ParseMap(a.Body)
	if err != nil {
		return nil
	}
	return m
}

// parseInlineMap decodes a data:application/json[;base64],… sourceMappingURL.
func parseInlineMap(smURL string) *Map {
	comma := strings.Index(smURL, ",")
	if comma < 0 {
		return nil
	}
	meta, payload := smURL[:comma], smURL[comma+1:]
	var raw []byte
	if strings.Contains(meta, ";base64") {
		decoded, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return nil
		}
		raw = decoded
	} else {
		if decoded, err := url.QueryUnescape(payload); err == nil {
			raw = []byte(decoded)
		}
	}
	m, err := ParseMap(raw)
	if err != nil {
		return nil
	}
	return m
}
