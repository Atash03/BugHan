package sourcemap

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Segment is one decoded mapping entry with absolute (delta-resolved) values.
// Generated and original positions are 0-based, matching the v3 spec.
type Segment struct {
	GenLine int
	GenCol  int
	SrcIdx  int // -1 when the segment carries no source (1-field form)
	SrcLine int
	SrcCol  int
	NameIdx int // -1 when absent
}

// Map is a parsed source map v3 document.
type Map struct {
	Version        int
	Sources        []string
	SourcesContent []*string // nil entries where content is absent
	Names          []string
	SourceRoot     string
	DebugID        string // non-standard "debugId" field used by Sentry tooling

	lines [][]Segment // per generated line, ascending by GenCol
}

// rawMap is the JSON document shape before VLQ decoding.
type rawMap struct {
	Version        int      `json:"version"`
	Sources        []string `json:"sources"`
	SourcesContent []*string `json:"sourcesContent"`
	Names          []string `json:"names"`
	SourceRoot     string   `json:"sourceRoot"`
	Mappings       string   `json:"mappings"`
	DebugID        string   `json:"debugId"`
}

// ParseMap decodes and validates a source map v3 document.
func ParseMap(data []byte) (*Map, error) {
	var raw rawMap
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("sourcemap JSON: %w", err)
	}
	if raw.Version != 3 {
		return nil, fmt.Errorf("unsupported sourcemap version %d", raw.Version)
	}
	m := &Map{
		Version:        raw.Version,
		Sources:        raw.Sources,
		SourcesContent: raw.SourcesContent,
		Names:          raw.Names,
		SourceRoot:     raw.SourceRoot,
		DebugID:        raw.DebugID,
	}
	if err := m.decodeMappings(raw.Mappings); err != nil {
		return nil, err
	}
	return m, nil
}

// decodeMappings walks the VLQ mapping string, resolving per-field deltas:
// generated columns reset per line; source index, source line, source column
// and name index persist across lines and segments.
func (m *Map) decodeMappings(s string) error {
	var srcIdx, srcLine, srcCol, nameIdx int64
	for line := 0; len(s) > 0; line++ {
		var rest string
		var segsPart string
		if i := indexByte(s, ';'); i >= 0 {
			segsPart, rest, s = s[:i], s[i+1:], s[i+1:]
		} else {
			segsPart, rest = s, ""
			s = ""
		}
		var genCol int64
		var segs []Segment
		for len(segsPart) > 0 {
			var segStr string
			if i := indexByte(segsPart, ','); i >= 0 {
				segStr, segsPart = segsPart[:i], segsPart[i+1:]
			} else {
				segStr, segsPart = segsPart, ""
			}
			if segStr == "" {
				continue
			}
			fields, err := decodeVLQSegment(segStr)
			if err != nil {
				return fmt.Errorf("mappings line %d: %w", line, err)
			}
			if len(fields) < 1 || len(fields) == 2 || len(fields) > 5 {
				return fmt.Errorf("mappings line %d: segment has %d fields", line, len(fields))
			}
			genCol += fields[0]
			seg := Segment{GenLine: line, GenCol: int(genCol), SrcIdx: -1, NameIdx: -1}
			if len(fields) >= 4 {
				srcIdx += fields[1]
				srcLine += fields[2]
				srcCol += fields[3]
				seg.SrcIdx = int(srcIdx)
				seg.SrcLine = int(srcLine)
				seg.SrcCol = int(srcCol)
				if len(fields) == 5 {
					nameIdx += fields[4]
					seg.NameIdx = int(nameIdx)
				}
			}
			segs = append(segs, seg)
		}
		m.lines = append(m.lines, segs)
		if rest == "" && len(segsPart) == 0 {
			break
		}
	}
	return nil
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// Lookup finds the mapping for a generated position (0-based): the segment
// with the greatest GenCol ≤ col on that line. ok=false when the line has no
// segments or all start after col.
func (m *Map) Lookup(line, col int) (Segment, bool) {
	if line < 0 || line >= len(m.lines) {
		return Segment{}, false
	}
	segs := m.lines[line]
	if len(segs) == 0 {
		return Segment{}, false
	}
	i := sort.Search(len(segs), func(i int) bool { return segs[i].GenCol > col })
	if i == 0 {
		return Segment{}, false
	}
	return segs[i-1], true
}
