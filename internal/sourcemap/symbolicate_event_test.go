package sourcemap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/Atash03/BugHan/internal/db"
	"github.com/Atash03/BugHan/internal/testdb"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedRelease creates an org/project/release and uploads artifacts directly
// (the HTTP upload surface is covered by httpapi tests).
func seedRelease(t *testing.T, pool *pgxpool.Pool, artifacts map[string]string, dists map[string]string) (projectID, release string) {
	t.Helper()
	ctx := context.Background()
	orgID, projID := uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO organizations (id, name, slug) VALUES ($1,'O','o')`, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO projects (id, org_id, name, slug, platform) VALUES ($1,$2,'P','p','javascript')`,
		projID, orgID); err != nil {
		t.Fatal(err)
	}
	release = "r-" + uuid.NewString()[:8]
	if _, err := pool.Exec(ctx,
		`INSERT INTO releases (id, project_id, version) VALUES (gen_random_uuid(),$1,$2)`,
		projID, release); err != nil {
		t.Fatal(err)
	}
	for name, body := range artifacts {
		sum := sha256.Sum256([]byte(body))
		debugID, smURL := ExtractArtifact(name, []byte(body))
		if _, err := pool.Exec(ctx, `
			INSERT INTO release_files (id, project_id, release, dist, name, body, size, sha256, debug_id, sourcemap_url)
			VALUES (gen_random_uuid(),$1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			projID, release, dists[name], name, []byte(body), len(body), hex.EncodeToString(sum[:]), debugID, smURL); err != nil {
		t.Fatal(err)
		}
	}
	return projID, release
}

const eventMapJSON = `{"version":3,"sources":["webpack:///src/app.ts"],"names":["onClick"],
"mappings":"AAAA,SAIEA","sourcesContent":["function boot() {\n  alert('hi');\n}\n\nfunction onClick() {\n  crash();\n}\n"]}`

// seedEvent inserts one error event carrying two frames over /assets/app.js.
func seedEvent(t *testing.T, pool *pgxpool.Pool, projectID, release, dist string, images string) (uuid.UUID, time.Time, []byte) {
	t.Helper()
	ctx := context.Background()
	if err := db.EnsurePartitions(ctx, pool, []time.Time{time.Now()}); err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	ts := time.Now().UTC().Truncate(time.Second)
	payload := map[string]any{
		"release": release, "dist": dist, "platform": "javascript",
		"exception": map[string]any{"values": []map[string]any{{
			"type": "ReferenceError", "value": "crash is not defined",
			"stacktrace": map[string]any{"frames": []map[string]any{
				{"filename": "app.js", "abs_path": "http://h.com/assets/app.js", "lineno": 1, "colno": 1, "in_app": true, "function": "t"},
				{"filename": "app.js", "abs_path": "http://h.com/assets/app.js", "lineno": 1, "colno": 10, "in_app": true, "function": "e"},
			}},
		}}},
	}
	if images != "" {
		payload["debug_meta"] = map[string]any{"images": json.RawMessage(images)}
	}
	raw, _ := json.Marshal(payload)
	if _, err := pool.Exec(ctx, `
		INSERT INTO events_part (id, project_id, timestamp, release, dist, payload)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		id, projectID, ts, release, dist, raw); err != nil {
		t.Fatal(err)
	}
	return id, ts, raw
}

func TestSymbolicateEventEndToEnd(t *testing.T) {
	pool := testdb.New(t)
	projectID, release := seedRelease(t, pool, map[string]string{
		"/assets/app.js":     "//# debugId=8e15901e-3eb2-4ba6-8b38-33aa5e97e3a3\n//# sourceMappingURL=app.js.map\nvar a=1",
		"/assets/app.js.map": eventMapJSON,
	}, nil)
	eventID, ts, raw := seedEvent(t, pool, projectID, release, "", "")

	ctx := context.Background()
	res, err := SymbolicateEvent(ctx, pool, projectID, eventID, ts, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Symbolicated {
		t.Fatalf("not symbolicated: %+v", res)
	}
	if len(res.Exceptions) != 1 || len(res.Exceptions[0].Frames) != 2 {
		t.Fatalf("exceptions = %+v", res.Exceptions)
	}
	top := res.Exceptions[0].Frames[1]
	if top.Source != "webpack:///src/app.ts" || top.SrcLine != 5 || top.SrcCol != 3 {
		t.Fatalf("top frame = %+v", top)
	}
	if top.Function != "onClick" {
		t.Fatalf("function = %q", top.Function)
	}
	// sourcesContent: the excerpt centers on the mapped line, with radius-2
	// context above and below.
	if top.Context == nil || top.Context.LineText != "function onClick() {" ||
		len(top.Context.Pre) != 2 || top.Context.Pre[1] != "}" ||
		len(top.Context.Post) != 2 || top.Context.Post[0] != "  crash();" {
		t.Fatalf("context = %+v", top.Context)
	}

	// Write-back: the event row now carries the result; a second call returns
	// the cached view without recomputing (same cache key, same result).
	var stored string
	if err := pool.QueryRow(ctx,
		`SELECT symbolicated::text FROM events_part WHERE id = $1 AND timestamp = $2`,
		eventID, ts).Scan(&stored); err != nil || stored == "" {
		t.Fatalf("write-back missing: %v %q", err, stored)
	}
	res2, err := SymbolicateEvent(ctx, pool, projectID, eventID, ts, raw)
	if err != nil || res2.CacheKey != res.CacheKey || !res2.Symbolicated {
		t.Fatalf("cached call = %+v %v", res2, err)
	}
}

func TestSymbolicateEventCacheInvalidation(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	projectID, release := seedRelease(t, pool, map[string]string{
		"/assets/app.js":     "//# sourceMappingURL=app.js.map\nvar a=1",
		"/assets/app.js.map": eventMapJSON,
	}, nil)
	eventID, ts, raw := seedEvent(t, pool, projectID, release, "", "")

	res1, err := SymbolicateEvent(ctx, pool, projectID, eventID, ts, raw)
	if err != nil || !res1.Symbolicated {
		t.Fatalf("first view = %+v %v", res1, err)
	}

	// Re-deploy: the map changes content (delete + upload is the sentry-cli
	// workflow). The cache key changes, so the next view re-symbolicates.
	if _, err := pool.Exec(ctx, `DELETE FROM release_files WHERE name = '/assets/app.js.map' AND project_id = $1`, projectID); err != nil {
		t.Fatal(err)
	}
	newMap := `{"version":3,"sources":["webpack:///src/renamed.ts"],"names":[],
"mappings":"AAAA,SAIE","sourcesContent":["function boot() {\n  alert('hi');\n}\n\nfunction onClick() {\n  crash();\n}\n"]}`
	sum := sha256.Sum256([]byte(newMap))
	if _, err := pool.Exec(ctx, `
		INSERT INTO release_files (id, project_id, release, dist, name, body, size, sha256)
		VALUES (gen_random_uuid(),$1,$2,'','/assets/app.js.map',$3,$4,$5)`,
		projectID, release, []byte(newMap), len(newMap), hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}

	res2, err := SymbolicateEvent(ctx, pool, projectID, eventID, ts, raw)
	if err != nil {
		t.Fatal(err)
	}
	if res2.CacheKey == res1.CacheKey {
		t.Fatal("cache key should change after re-upload")
	}
	if got := res2.Exceptions[0].Frames[1].Source; got != "webpack:///src/renamed.ts" {
		t.Fatalf("stale cache: source = %q", got)
	}
}

func TestSymbolicateEventMissingMapHint(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	// The release has an unrelated artifact only; the frame's file was never
	// uploaded with a map.
	projectID, release := seedRelease(t, pool, map[string]string{
		"/assets/other.js": "var x=1",
	}, nil)
	eventID, ts, raw := seedEvent(t, pool, projectID, release, "", "")

	res, err := SymbolicateEvent(ctx, pool, projectID, eventID, ts, raw)
	if err != nil {
		t.Fatal(err)
	}
	if res.Symbolicated {
		t.Fatalf("nothing should resolve: %+v", res)
	}
	if len(res.MissingMaps) != 1 {
		t.Fatalf("hints = %+v", res.MissingMaps)
	}
	h := res.MissingMaps[0]
	if h.AbsPath != "http://h.com/assets/app.js" || h.Release != release || h.Reason != "no source map found" {
		t.Fatalf("hint = %+v", h)
	}
	// Frames stay minified.
	if f := res.Exceptions[0].Frames[0]; f.Resolved || f.Colno != 1 {
		t.Fatalf("frame = %+v", f)
	}
}

func TestSymbolicateEventNoRelease(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	projectID, _ := seedRelease(t, pool, nil, nil)
	id := uuid.New()
	ts := time.Now().UTC().Truncate(time.Second)
	raw, _ := json.Marshal(map[string]any{
		"platform": "javascript",
		"exception": map[string]any{"values": []map[string]any{{
			"type": "Error", "value": "boom",
			"stacktrace": map[string]any{"frames": []map[string]any{
				{"filename": "a.js", "lineno": 1, "colno": 1},
			}},
		}}},
	})
	if err := db.EnsurePartitions(ctx, pool, []time.Time{time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO events_part (id, project_id, timestamp, release, dist, payload) VALUES ($1,$2,$3,'','', $4)`,
		id, projectID, ts, raw); err != nil {
		t.Fatal(err)
	}
	res, err := SymbolicateEvent(ctx, pool, projectID, id, ts, raw)
	if err != nil || !res.NoRelease || res.Symbolicated {
		t.Fatalf("no-release = %+v %v", res, err)
	}
	// Cached on the row: a repeat call returns the same view.
	res2, err := SymbolicateEvent(ctx, pool, projectID, id, ts, raw)
	if err != nil || !res2.NoRelease {
		t.Fatalf("cached no-release = %+v %v", res2, err)
	}
}

func TestSymbolicateEventDebugMetaPriority(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	// The js artifact's sourceMappingURL points at the WRONG map; the event's
	// debug_meta.images debug id points at the right one, which must win.
	projectID, release := seedRelease(t, pool, map[string]string{
		"/assets/app.js":     "//# sourceMappingURL=wrong.js.map\nvar a=1",
		"/assets/wrong.js.map": `{"version":3,"sources":["w.ts"],"mappings":"AAAA"}`,
		"/assets/right.js.map": `{"version":3,"sources":["r.ts"],"mappings":"AAAA",
			"debugId":"8e15901e-3eb2-4ba6-8b38-33aa5e97e3a3"}`,
	}, nil)
	eventID, ts, raw := seedEvent(t, pool, projectID, release, "",
		`[{"type":"sourcemap","debug_id":"8e15901e-3eb2-4ba6-8b38-33aa5e97e3a3","code_file":"http://h.com/assets/app.js"}]`)

	res, err := SymbolicateEvent(ctx, pool, projectID, eventID, ts, raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Exceptions[0].Frames[0].Source; got != "r.ts" {
		t.Fatalf("debug_meta did not win: source = %q", got)
	}
}
