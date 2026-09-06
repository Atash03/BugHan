package sourcemap

import "strings"

// Frame is one minified stack frame as SDKs send it (1-based positions).
type Frame struct {
	Lineno   int    // 1-based generated line; 0 = unknown
	Colno    int    // 1-based generated column; 0 = unknown
	AbsPath  string `json:"abs_path,omitempty"`
	Filename string `json:"filename,omitempty"`
	Function string `json:"function,omitempty"`
}

// Snippet is the inline code excerpt rendered from sourcesContent.
type Snippet struct {
	Path     string
	Line     int      // 1-based line the excerpt is centered on
	Pre      []string // lines above, nearest first
	LineText string
	Post     []string // lines below, nearest first
}

// FrameResult is the symbolicated view of one minified frame.
type FrameResult struct {
	Resolved bool
	Source   string // original source path (sourceRoot applied)
	SrcLine  int    // 1-based
	SrcCol   int    // 1-based
	Function string // original name from the map, when present
	Context  *Snippet
}

// snippetRadius is how many lines of pre/post context come from sourcesContent.
const snippetRadius = 2

// SymbolicateFrame maps one minified frame onto a parsed source map.
// Unmappable frames (unknown position, line outside the map) resolve to
// Resolved=false with the map's view absent — callers keep the minified frame.
func SymbolicateFrame(m *Map, f Frame) FrameResult {
	if f.Lineno <= 0 || f.Colno <= 0 {
		return FrameResult{}
	}
	seg, ok := m.Lookup(f.Lineno-1, f.Colno-1)
	if !ok || seg.SrcIdx < 0 || seg.SrcIdx >= len(m.Sources) {
		return FrameResult{}
	}
	r := FrameResult{
		Resolved: true,
		Source:   joinSourceRoot(m.SourceRoot, m.Sources[seg.SrcIdx]),
		SrcLine:  seg.SrcLine + 1,
		SrcCol:   seg.SrcCol + 1,
	}
	if seg.NameIdx >= 0 && seg.NameIdx < len(m.Names) {
		r.Function = m.Names[seg.NameIdx]
	}
	if seg.SrcIdx < len(m.SourcesContent) && m.SourcesContent[seg.SrcIdx] != nil {
		r.Context = snippetAt(r.Source, *m.SourcesContent[seg.SrcIdx], r.SrcLine)
	}
	return r
}

// joinSourceRoot applies the map's sourceRoot per the v3 spec: a prefix,
// joined with "/" when both sides need it.
func joinSourceRoot(root, source string) string {
	root = strings.TrimSuffix(root, "/")
	if root == "" {
		return source
	}
	if strings.HasPrefix(source, "/") {
		return root + source
	}
	return root + "/" + source
}

// snippetAt renders the code excerpt around a 1-based line.
func snippetAt(path, content string, line int) *Snippet {
	lines := strings.Split(content, "\n")
	if line < 1 || line > len(lines) {
		return nil
	}
	s := &Snippet{Path: path, Line: line, LineText: lines[line-1]}
	for i := line - 2; i >= 0 && len(s.Pre) < snippetRadius; i-- {
		s.Pre = append([]string{lines[i]}, s.Pre...)
	}
	for i := line; i < len(lines) && len(s.Post) < snippetRadius; i++ {
		s.Post = append(s.Post, lines[i])
	}
	return s
}
