package sourcemap

import (
	"strings"
	"testing"
)

// linesFixture: a.ts maps (1,1)→0:0 and (1,10)→5:3, generated line 2→3:6.
const linesFixture = `{"version":3,"sources":["a.ts"],"names":[],
"mappings":"AAAA,SAIE;AAFG",
"sourcesContent":["zero\none\ntwo\nthree\nfour\nfive\nsix"]}`

func TestSymbolicateFrameBasics(t *testing.T) {
	m, err := ParseMap([]byte(linesFixture))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	r := SymbolicateFrame(m, Frame{Lineno: 1, Colno: 1, AbsPath: "http://h/app.js"})
	if !r.Resolved || r.Source != "a.ts" || r.SrcLine != 1 || r.SrcCol != 1 {
		t.Fatalf("frame = %+v", r)
	}
	if r.Context == nil || r.Context.LineText != "zero" || len(r.Context.Pre) != 0 || len(r.Context.Post) != 2 {
		t.Fatalf("context = %+v", r.Context)
	}

	r = SymbolicateFrame(m, Frame{Lineno: 1, Colno: 10})
	if !r.Resolved || r.SrcLine != 5 || r.SrcCol != 3 {
		t.Fatalf("frame(1,10) = %+v", r)
	}

	r = SymbolicateFrame(m, Frame{Lineno: 2, Colno: 1})
	if !r.Resolved || r.SrcLine != 3 || r.SrcCol != 6 {
		t.Fatalf("frame(2,1) = %+v", r)
	}
}

func TestSymbolicateFrameSourceRootAndName(t *testing.T) {
	m, err := ParseMap([]byte(`{"version":3,"sources":["src/app.ts"],"names":["onClick"],
		"sourceRoot":"webpack:///","mappings":"AAAAA"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	r := SymbolicateFrame(m, Frame{Lineno: 1, Colno: 1})
	if !r.Resolved || r.Source != "webpack:///src/app.ts" {
		t.Fatalf("sourceRoot frame = %+v", r)
	}
	if r.Function != "onClick" {
		t.Fatalf("function = %q", r.Function)
	}
}

func TestSymbolicateFrameUnresolved(t *testing.T) {
	m, err := ParseMap([]byte(linesFixture))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// A line the map doesn't cover stays unresolved.
	r := SymbolicateFrame(m, Frame{Lineno: 99, Colno: 1})
	if r.Resolved || r.Source != "" {
		t.Fatalf("frame(99,1) = %+v", r)
	}
	// SDKs send 0 when the position is unknown.
	r = SymbolicateFrame(m, Frame{Lineno: 0, Colno: 0})
	if r.Resolved {
		t.Fatalf("zero position = %+v", r)
	}
}

func TestSymbolicateFrameNoSourcesContent(t *testing.T) {
	m, err := ParseMap([]byte(`{"version":3,"sources":["a.ts"],"mappings":"AAAA"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	r := SymbolicateFrame(m, Frame{Lineno: 1, Colno: 1})
	if !r.Resolved || r.Context != nil {
		t.Fatalf("no-content frame = %+v", r)
	}
}

// snippetAt is exercised indirectly through SymbolicateFrame; test clipping
// near the start and end of the source.
func TestSnippetClipping(t *testing.T) {
	m, err := ParseMap([]byte(`{"version":3,"sources":["a.ts"],
		"mappings":"AAAA;AACA;AACA;AACA;AACA;AACA",
		"sourcesContent":["l0\nl1\nl2\nl3\nl4\nl5"]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	first := SymbolicateFrame(m, Frame{Lineno: 1, Colno: 1})
	if first.Context.LineText != "l0" || len(first.Context.Pre) != 0 ||
		len(first.Context.Post) != 2 || first.Context.Post[1] != "l2" {
		t.Fatalf("head snippet = %+v", first.Context)
	}
	last := SymbolicateFrame(m, Frame{Lineno: 6, Colno: 1})
	if last.Context.LineText != "l5" || len(last.Context.Pre) != 2 || len(last.Context.Post) != 0 {
		t.Fatalf("tail snippet = %+v", last.Context)
	}
	if !strings.HasSuffix(last.Context.Path, "a.ts") {
		t.Fatalf("snippet path = %q", last.Context.Path)
	}
}
