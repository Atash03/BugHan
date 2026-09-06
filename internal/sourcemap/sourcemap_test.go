package sourcemap

import "testing"

// The fixture map encodes, by hand:
//   generated line 0: col 0 → a.ts:0:0, col 9 → a.ts:4:2
//   generated line 1: col 0 → a.ts:2:5
// as VLQ segments "AAAA,SAIE;AAFG" ([9,0,4,2] = 9→S, 0→A, 4→I, 2→E).
const fixtureMap = `{"version":3,"sources":["a.ts"],"names":[],"mappings":"AAAA,SAIE;AAFG",
"sourcesContent":["zero\none\ntwo\nthree\nfour\nfive"]}`

func TestParseMapLookup(t *testing.T) {
	m, err := ParseMap([]byte(fixtureMap))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(m.Sources) != 1 || m.Sources[0] != "a.ts" {
		t.Fatalf("sources = %v", m.Sources)
	}

	cases := []struct {
		line, col                    int // 0-based generated
		src                          string
		srcLine, srcCol              int // 0-based original
	}{
		{0, 0, "a.ts", 0, 0},
		{0, 9, "a.ts", 4, 2},
		// Columns past the last segment resolve to the nearest preceding one.
		{0, 15, "a.ts", 4, 2},
		{1, 0, "a.ts", 2, 5},
	}
	for _, c := range cases {
		seg, ok := m.Lookup(c.line, c.col)
		if !ok {
			t.Fatalf("lookup(%d,%d): no segment", c.line, c.col)
		}
		if seg.SrcIdx != 0 || seg.SrcLine != c.srcLine || seg.SrcCol != c.srcCol {
			t.Fatalf("lookup(%d,%d) = %+v, want a.ts %d:%d", c.line, c.col, seg, c.srcLine, c.srcCol)
		}
	}

	// No mapping on a line the map doesn't cover.
	if _, ok := m.Lookup(5, 0); ok {
		t.Fatal("lookup(5,0) should miss")
	}
}

func TestVLQDecode(t *testing.T) {
	cases := map[string][]int64{
		"AAAA": {0, 0, 0, 0},
		"SAIE": {9, 0, 4, 2},
		"AAFG": {0, 0, -2, 3},
		"C":    {1},
		"D":    {-1},
		"gB":   {16},
	}
	for in, want := range cases {
		got, err := decodeVLQSegment(in)
		if err != nil {
			t.Fatalf("decode %q: %v", in, err)
		}
		if len(got) != len(want) {
			t.Fatalf("decode %q = %v, want %v", in, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("decode %q = %v, want %v", in, got, want)
			}
		}
	}
	if _, err := decodeVLQSegment("!!!"); err == nil {
		t.Fatal("garbage should error")
	}
}

func TestParseMapRejectsNonV3(t *testing.T) {
	if _, err := ParseMap([]byte(`{"version":4,"sources":[],"mappings":""}`)); err == nil {
		t.Fatal("version 4 should be rejected")
	}
	if _, err := ParseMap([]byte(`not json`)); err == nil {
		t.Fatal("garbage should be rejected")
	}
}

func TestParseMapNamesAndSourceRoot(t *testing.T) {
	m, err := ParseMap([]byte(`{"version":3,"sources":["src/app.ts"],"names":["onClick"],
		"sourceRoot":"webpack:///","mappings":"AAAAA"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	seg, ok := m.Lookup(0, 0)
	if !ok || seg.NameIdx != 0 {
		t.Fatalf("lookup = %+v ok=%v", seg, ok)
	}
	if m.SourceRoot != "webpack:///" || len(m.Names) != 1 || m.Names[0] != "onClick" {
		t.Fatalf("map = %+v", m)
	}
}
