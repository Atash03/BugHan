package sourcemap

import (
	"encoding/base64"
	"strings"
	"testing"
)

func fixtureIndex() *Index {
	return &Index{Artifacts: []Artifact{
		{
			Name:         "/assets/app.js",
			SourcemapURL: "app.js.map",
			DebugID:      "8e15901e-3eb2-4ba6-8b38-33aa5e97e3a3",
			Body:         []byte("//# sourceMappingURL=app.js.map\n"),
		},
		{Name: "/assets/app.js.map", Body: []byte(`{"version":3,"sources":["a.ts"],"mappings":"AAAA"}`)},
		{
			Name:         "/assets/vendor.js",
			SourcemapURL: "data:application/json;base64," + base64.StdEncoding.EncodeToString(
				[]byte(`{"version":3,"sources":["v.ts"],"mappings":"AAAA"}`)),
		},
		{Name: "/assets/noref.js", Body: []byte("console.log(1)")},
		{Name: "/assets/orphan.js.map", Body: []byte(`{"version":3,"sources":["o.ts"],"mappings":"AAAA"}`)},
		{Name: "/assets/dist-tagged.js", Dist: "ios", Body: []byte("")},
		{Name: "/assets/dist-tagged.js.map", Dist: "ios", Body: []byte(`{"version":3,"sources":["d.ts"],"mappings":"AAAA"}`)},
	}}
}

func TestResolveViaSourceMappingURL(t *testing.T) {
	ix := fixtureIndex()
	// Query/fragment are stripped before matching.
	art, m := ix.ResolveMap("http://h.com/assets/app.js?v=42#frag", nil)
	if art == nil || art.Name != "/assets/app.js.map" {
		t.Fatalf("artifact = %v", art)
	}
	if m == nil || len(m.Sources) != 1 || m.Sources[0] != "a.ts" {
		t.Fatalf("map = %+v", m)
	}
}

func TestResolveInlineDataURL(t *testing.T) {
	ix := fixtureIndex()
	art, m := ix.ResolveMap("/assets/vendor.js", nil)
	if art == nil || art.Name != "/assets/vendor.js" {
		t.Fatalf("artifact = %v", art)
	}
	if m == nil || len(m.Sources) != 1 || m.Sources[0] != "v.ts" {
		t.Fatalf("inline map = %+v", m)
	}
}

func TestResolveByDebugIDFromArtifact(t *testing.T) {
	// The js artifact carries a debugId but its map was uploaded without the
	// sourceMappingURL chain being intact: a map artifact with the same
	// debug_id is found by identity, not by name.
	ix := &Index{Artifacts: []Artifact{
		{Name: "/x.js", DebugID: "11111111-2222-4333-8444-555555555555"},
		{Name: "/x.min.js.map", DebugID: "11111111-2222-4333-8444-555555555555",
			Body: []byte(`{"version":3,"sources":["x.ts"],"mappings":"AAAA"}`)},
	}}
	art, m := ix.ResolveMap("/x.js", nil)
	if art == nil || m == nil || m.Sources[0] != "x.ts" {
		t.Fatalf("debug_id resolve = %v %v", art, m)
	}
}

func TestResolveDebugMetaImagesWin(t *testing.T) {
	// debug_meta.images are consulted first: the image's debug id points at a
	// map that is NOT the sourceMappingURL target.
	ix := &Index{Artifacts: []Artifact{
		{Name: "/a.js", SourcemapURL: "wrong.js.map"},
		{Name: "/wrong.js.map", Body: []byte(`{"version":3,"sources":["wrong.ts"],"mappings":"AAAA"}`)},
		{Name: "/right.js.map", DebugID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
			Body: []byte(`{"version":3,"sources":["right.ts"],"mappings":"AAAA"}`)},
	}}
	art, m := ix.ResolveMap("/a.js", []string{"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"})
	if m == nil || len(m.Sources) != 1 || m.Sources[0] != "right.ts" {
		t.Fatalf("images-first resolve = %v %v", art, m)
	}
}

func TestResolveByPlusDotMapFallback(t *testing.T) {
	// Only the map was uploaded; the minified file itself wasn't.
	ix := fixtureIndex()
	art, m := ix.ResolveMap("/assets/orphan.js", nil)
	if art == nil || m == nil || m.Sources[0] != "o.ts" {
		t.Fatalf("+.map fallback = %v %v", art, m)
	}
}

func TestResolveMissing(t *testing.T) {
	ix := fixtureIndex()
	art, m := ix.ResolveMap("/assets/unknown.js", nil)
	if art != nil || m != nil {
		t.Fatalf("missing resolve = %v %v", art, m)
	}
}

func TestResolveDistPreference(t *testing.T) {
	ix := fixtureIndex()
	ix.Dist = "ios"
	art, m := ix.ResolveMap("/assets/dist-tagged.js", nil)
	if art == nil || art.Name != "/assets/dist-tagged.js.map" {
		t.Fatalf("dist-tagged = %v", art)
	}

	// An event dist with no matching artifacts falls back to bare uploads.
	ix.Dist = "android"
	art, m = ix.ResolveMap("/assets/app.js", nil)
	if art == nil || m == nil || m.Sources[0] != "a.ts" {
		t.Fatalf("bare fallback = %v %v", art, m)
	}

	// A bare event never sees dist-tagged artifacts.
	ix.Dist = ""
	art, _ = ix.ResolveMap("/assets/dist-tagged.js", nil)
	if art != nil {
		t.Fatalf("bare event saw dist artifact: %v", art)
	}
}

func TestCandidateNamesStripsScheme(t *testing.T) {
	// An artifact uploaded under a bare path matches a full-URL frame.
	ix := &Index{Artifacts: []Artifact{
		{Name: "app.js.map", Body: []byte(`{"version":3,"sources":["a.ts"],"mappings":"AAAA"}`)},
		{Name: "app.js", SourcemapURL: "app.js.map"},
	}}
	art, m := ix.ResolveMap("https://cdn.example.com/static/app.js", nil)
	if art == nil || m == nil {
		t.Fatalf("scheme-stripped match = %v %v", art, m)
	}
	if !strings.HasSuffix(art.Name, "app.js.map") {
		t.Fatalf("artifact = %v", art)
	}
}
