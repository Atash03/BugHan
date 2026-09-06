package sourcemap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// resultV is the persisted symbolicated-result format version; bump when the
// EventResult shape changes so old caches are treated as stale.
const resultV = 1

// missingMapCap bounds the missing-map hints stored per event.
const missingMapCap = 20

// EventResult is the symbolicated view of one error event, persisted on the
// event row (events_part.symbolicated) as a write-back cache.
type EventResult struct {
	V            int               `json:"v"`
	CacheKey     string            `json:"cache_key"`
	Symbolicated bool              `json:"symbolicated"`
	NoRelease    bool              `json:"no_release,omitempty"`
	Exceptions   []ExceptionResult `json:"exceptions,omitempty"`
	MissingMaps  []MissingMap      `json:"missing_maps,omitempty"`
}

// ExceptionResult mirrors one entry of the payload's exception.values array.
type ExceptionResult struct {
	Type   string      `json:"type,omitempty"`
	Value  string      `json:"value,omitempty"`
	Frames []FrameView `json:"frames"`
}

// FrameView is one stack frame: the minified position as received plus the
// symbolicated overlay when a map resolved it.
type FrameView struct {
	Lineno   int    `json:"lineno"`
	Colno    int    `json:"colno"`
	AbsPath  string `json:"abs_path,omitempty"`
	Filename string `json:"filename,omitempty"`
	InApp    bool   `json:"in_app,omitempty"`
	Function string `json:"function,omitempty"` // original name, else the minified one

	// Symbolicated overlay (empty when unresolved).
	Source   string   `json:"source,omitempty"`
	SrcLine  int      `json:"src_line,omitempty"`
	SrcCol   int      `json:"src_col,omitempty"`
	Context  *Snippet `json:"context,omitempty"`
	Resolved bool     `json:"resolved,omitempty"`
}

// MissingMap records a minified frame no artifact could symbolicate; the UI
// renders these as "no source map found" hints naming release and abs_path.
type MissingMap struct {
	AbsPath string `json:"abs_path"`
	Release string `json:"release"`
	Dist    string `json:"dist,omitempty"`
	Reason  string `json:"reason"`
}

// SymbolicateEvent resolves one stored event's stack frames against the
// project's uploaded release artifacts. Results are cached on the event row
// keyed by a hash of the release's artifact set, so re-uploading a map
// invalidates the cache and the next view re-symbolicates.
func SymbolicateEvent(ctx context.Context, pool *pgxpool.Pool, projectID string, eventID uuid.UUID, eventTS time.Time, rawPayload []byte) (*EventResult, error) {
	var payload struct {
		Release string `json:"release"`
		Dist    string `json:"dist"`
		DebugMeta struct {
			Images []struct {
				DebugID  string `json:"debug_id"`
				CodeFile string `json:"code_file"`
			} `json:"images"`
		} `json:"debug_meta"`
		Exceptions struct {
			Values []struct {
				Type       string `json:"type"`
				Value      string `json:"value"`
				Stacktrace struct {
					Frames []struct {
						Lineno   int    `json:"lineno"`
						Colno    int    `json:"colno"`
						AbsPath  string `json:"abs_path"`
						Filename string `json:"filename"`
						Function string `json:"function"`
						InApp    *bool  `json:"in_app"`
					} `json:"frames"`
				} `json:"stacktrace"`
			} `json:"values"`
		} `json:"exception"`
	}
	if err := json.Unmarshal(rawPayload, &payload); err != nil {
		return nil, err
	}

	arts, err := loadArtifacts(ctx, pool, projectID, payload.Release)
	if err != nil {
		return nil, err
	}
	cacheKey := artifactSetKey(payload.Release, arts)

	// Cache hit: the stored result was computed against the same artifacts.
	var cached *EventResult
	var stored []byte
	if err := pool.QueryRow(ctx,
		`SELECT symbolicated FROM events_part WHERE id = $1 AND timestamp = $2`,
		eventID, eventTS).Scan(&stored); err == nil && len(stored) > 0 {
		var r EventResult
		if json.Unmarshal(stored, &r) == nil && r.V == resultV && r.CacheKey == cacheKey {
			cached = &r
		}
	}
	if cached != nil {
		return cached, nil
	}

	res := &EventResult{V: resultV, CacheKey: cacheKey}
	if payload.Release == "" {
		res.NoRelease = true
		return res, writeBack(ctx, pool, eventID, eventTS, res)
	}

	ix := &Index{Artifacts: arts, Dist: payload.Dist}
	seenMissing := map[string]bool{}
	for _, exc := range payload.Exceptions.Values {
		er := ExceptionResult{Type: exc.Type, Value: exc.Value}
		for _, f := range exc.Stacktrace.Frames {
			fv := FrameView{
				Lineno: f.Lineno, Colno: f.Colno,
				AbsPath: f.AbsPath, Filename: f.Filename,
				Function: f.Function, InApp: f.InApp != nil && *f.InApp,
			}
			imageIDs := imageDebugIDs(payload.DebugMeta.Images, f.AbsPath, f.Filename)
			_, m := ix.ResolveMap(firstNonEmpty(f.AbsPath, f.Filename), imageIDs)
			if m == nil {
				if f.AbsPath != "" && !seenMissing[f.AbsPath] && len(res.MissingMaps) < missingMapCap {
					seenMissing[f.AbsPath] = true
					res.MissingMaps = append(res.MissingMaps, MissingMap{
						AbsPath: f.AbsPath, Release: payload.Release, Dist: payload.Dist,
						Reason: "no source map found",
					})
				}
				er.Frames = append(er.Frames, fv)
				continue
			}
			r := SymbolicateFrame(m, Frame{
				Lineno: f.Lineno, Colno: f.Colno,
				AbsPath: f.AbsPath, Filename: f.Filename, Function: f.Function,
			})
			if r.Resolved {
				fv.Resolved = true
				fv.Source, fv.SrcLine, fv.SrcCol = r.Source, r.SrcLine, r.SrcCol
				if r.Function != "" {
					fv.Function = r.Function
				}
				fv.Context = r.Context
				res.Symbolicated = true
			}
			er.Frames = append(er.Frames, fv)
		}
		res.Exceptions = append(res.Exceptions, er)
	}
	return res, writeBack(ctx, pool, eventID, eventTS, res)
}

// artifactRow is the metadata + (lazy) body of one release artifact.
type artifactRow struct {
	Artifact
	id       string
	hasBody  bool
	rawBody  []byte
	isMapish bool
}

// loadArtifacts fetches the release's artifact metadata; bodies load only for
// artifacts that can serve as a sourcemap (a .map, an inline data: holder, or
// debug-id identified) — minified code bodies never leave Postgres here.
func loadArtifacts(ctx context.Context, pool *pgxpool.Pool, projectID, release string) ([]Artifact, error) {
	if release == "" {
		return nil, nil
	}
	rows, err := pool.Query(ctx, `
		SELECT id::text, name, dist, sha256, debug_id, sourcemap_url
		FROM release_files WHERE project_id = $1 AND release = $2`, projectID, release)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var arts []artifactRow
	var needBody []string
	for rows.Next() {
		var a artifactRow
		var dist string
		if err := rows.Scan(&a.id, &a.Name, &dist, &a.SHA256, &a.DebugID, &a.SourcemapURL); err != nil {
			return nil, err
		}
		a.Dist = dist
		a.isMapish = a.DebugID != "" || hasSuffixFold(a.Name, ".map") ||
			hasPrefixFold(a.SourcemapURL, "data:")
		arts = append(arts, a)
		if a.isMapish {
			needBody = append(needBody, a.id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(needBody) > 0 {
		brows, err := pool.Query(ctx, `
			SELECT id::text, body FROM release_files WHERE id = ANY($1)`, needBody)
		if err != nil {
			return nil, err
		}
		defer brows.Close()
		bodies := map[string][]byte{}
		for brows.Next() {
			var id string
			var body []byte
			if err := brows.Scan(&id, &body); err != nil {
				return nil, err
			}
			bodies[id] = body
		}
		if err := brows.Err(); err != nil {
			return nil, err
		}
		for i := range arts {
			if b, ok := bodies[arts[i].id]; ok {
				arts[i].rawBody = b
				arts[i].hasBody = true
			}
		}
	}
	out := make([]Artifact, len(arts))
	for i, a := range arts {
		out[i] = a.Artifact
		if a.hasBody {
			out[i].Body = a.rawBody
		}
	}
	return out, nil
}

// artifactSetKey hashes the artifact set of a release so any upload or delete
// produces a new cache key.
func artifactSetKey(release string, arts []Artifact) string {
	type row struct{ dist, name, sha string }
	rs := make([]row, 0, len(arts))
	for _, a := range arts {
		rs = append(rs, row{a.Dist, a.Name, a.SHA256})
	}
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].name != rs[j].name {
			return rs[i].name < rs[j].name
		}
		return rs[i].dist < rs[j].dist
	})
	h := sha256.New()
	h.Write([]byte(release))
	h.Write([]byte{0})
	for _, r := range rs {
		h.Write([]byte(r.dist))
		h.Write([]byte{0x1f})
		h.Write([]byte(r.name))
		h.Write([]byte{0x1f})
		h.Write([]byte(r.sha))
		h.Write([]byte{0x1e})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func imageDebugIDs(images []struct {
	DebugID  string `json:"debug_id"`
	CodeFile string `json:"code_file"`
}, absPath, filename string) []string {
	if len(images) == 0 {
		return nil
	}
	want := normalizePath(firstNonEmpty(absPath, filename))
	if want == "" {
		return nil
	}
	var ids []string
	for _, img := range images {
		cf := normalizePath(img.CodeFile)
		if cf != "" && (cf == want || pathBase(cf) == pathBase(want)) {
			ids = append(ids, img.DebugID)
		}
	}
	return ids
}

func writeBack(ctx context.Context, pool *pgxpool.Pool, eventID uuid.UUID, ts time.Time, res *EventResult) error {
	raw, err := json.Marshal(res)
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx,
		`UPDATE events_part SET symbolicated = $1 WHERE id = $2 AND timestamp = $3`,
		raw, eventID, ts)
	return err
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func hasSuffixFold(s, suf string) bool {
	return len(s) >= len(suf) && toLowerASCII(s[len(s)-len(suf):]) == toLowerASCII(suf)
}

func hasPrefixFold(s, pre string) bool {
	return len(s) >= len(pre) && toLowerASCII(s[:len(pre)]) == toLowerASCII(pre)
}

func toLowerASCII(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 32
		}
	}
	return string(b)
}
