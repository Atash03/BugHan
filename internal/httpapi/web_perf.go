package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Atash03/BugHan/internal/auth"
	"github.com/Atash03/BugHan/internal/perf"
	"github.com/Atash03/BugHan/internal/sourcemap"
	"github.com/Atash03/BugHan/internal/web"
	"github.com/google/uuid"
)

// T9 web UI slice (DESIGN.md §11, §13): performance pages, trace view,
// releases pages, and the project feedback list. Server-rendered on the
// /{org}/{project}/... family, reusing the T6 perf query layer, the T7
// release-files store, and the pipeline's feedback rows. Plain HTML forms;
// no JS required except the release upload form (plain multipart POST).

// --- performance summary ---------------------------------------------------

// perfRowView is a TransactionSummaryRow with a precomputed failure rate.
type perfRowView struct {
	perf.TransactionSummaryRow
	FailRate float64
}

// vitalChip is one web-vital aggregate for the strip.
type vitalChip struct {
	Name   string
	Value  float64
	Rating string
	Unit   string
}

func vitalUnit(name string) string {
	if name == "cls" {
		return ""
	}
	return "ms"
}

func (s *Server) uiPerformance(w http.ResponseWriter, r *http.Request) {
	u := s.uiUser(w, r)
	if u == nil {
		return
	}
	o, p := s.uiProject(r)
	if o == nil {
		s.uiError(w, r, http.StatusNotFound, "Project not found.", nil)
		return
	}
	// ?name= selects the transaction detail view on the same route
	// (transaction names contain slashes, so a query param beats a segment).
	if strings.TrimSpace(r.URL.Query().Get("name")) != "" {
		s.uiTransactionDetail(w, r, u, o, p)
		return
	}
	since, until := statsPeriod(r)
	ctx := r.Context()

	summary, err := perf.TransactionSummary(ctx, s.pool, p.ID, since, until)
	if err != nil {
		s.uiError(w, r, 500, err.Error(), nil)
		return
	}
	rows := make([]perfRowView, 0, len(summary))
	for _, sm := range summary {
		v := perfRowView{TransactionSummaryRow: sm}
		if sm.Count > 0 {
			v.FailRate = float64(sm.Failures) * 100 / float64(sm.Count)
		}
		rows = append(rows, v)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Count > rows[j].Count })

	web.Render(w, 200, "app_performance", web.PageData{
		Title: "Performance",
		Data: map[string]any{
			"nav": s.uiNav(r, u, o, p, "performance"), "org": o, "project": p,
			"rows": rows, "period": r.URL.Query().Get("statsPeriod"),
			"vitals": s.uiVitalsStrip(r, p.ID),
			"chart":  s.uiTxChart(r, p.ID),
		},
	})
}

// uiVitalsStrip aggregates the six known web vitals over the 200 most recent
// transactions (median per vital) for the performance page strip.
func (s *Server) uiVitalsStrip(r *http.Request, projectID string) []vitalChip {
	rows, err := s.pool.Query(r.Context(), `
		SELECT measurements FROM transactions_part
		WHERE project_id = $1 ORDER BY start_ts DESC LIMIT 200`, projectID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	vals := map[string][]float64{}
	for rows.Next() {
		var m json.RawMessage
		_ = rows.Scan(&m)
		for k, v := range perf.ExtractWebVitals(m) {
			vals[k] = append(vals[k], v)
		}
	}
	rows.Close()
	out := []vitalChip{}
	for _, name := range perf.KnownVitals {
		vs := vals[name]
		if len(vs) == 0 {
			continue
		}
		sort.Float64s(vs)
		med := vs[len(vs)/2]
		out = append(out, vitalChip{
			Name: strings.ToUpper(name), Value: med,
			Rating: perf.VitalRating(name, med), Unit: vitalUnit(name),
		})
	}
	return out
}

// txChartBucket is one hour of transaction volume for the perf page chart.
type txChartBucket struct {
	Hour  string
	Count int64
}

func (s *Server) uiTxChart(r *http.Request, projectID string) map[string]any {
	type raw struct {
		Hour  string
		Count int64
	}
	chart := []raw{}
	rows, err := s.pool.Query(r.Context(), `
		SELECT date_trunc('hour', hour)::text, sum(count) FROM transaction_rollups
		WHERE project_id = $1 AND hour > now() - interval '24 hours'
		GROUP BY 1 ORDER BY 1`, projectID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var b raw
			_ = rows.Scan(&b.Hour, &b.Count)
			chart = append(chart, b)
		}
		rows.Close()
	}
	var max int64 = 1
	for _, b := range chart {
		if b.Count > max {
			max = b.Count
		}
	}
	return map[string]any{"buckets": chart, "max": max}
}

// --- transaction detail ----------------------------------------------------

func (s *Server) uiTransactionDetail(w http.ResponseWriter, r *http.Request, u *auth.User, o *org, p *project) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	since, until := statsPeriod(r)
	detail, err := perf.TransactionDetail(r.Context(), s.pool, p.ID, name, since, until)
	if err != nil {
		s.uiError(w, r, 500, err.Error(), nil)
		return
	}
	var histMax int64 = 1
	for _, b := range detail.Histogram {
		if b.Count > histMax {
			histMax = b.Count
		}
	}
	var failRate float64
	if detail.Count > 0 {
		failRate = float64(detail.Failures) * 100 / float64(detail.Count)
	}
	web.Render(w, 200, "app_transaction", web.PageData{
		Title: name,
		Data: map[string]any{
			"nav": s.uiNav(r, u, o, p, "performance"), "org": o, "project": p,
			"detail": detail, "hist_max": histMax, "fail_rate": failRate,
			"period": r.URL.Query().Get("statsPeriod"),
		},
	})
}

// --- trace view ------------------------------------------------------------

// traceTxView is one transaction row with server-computed waterfall geometry
// (percent offsets into the trace window) and per-span self-time.
type traceTxView struct {
	Tx     perf.TraceTransaction
	Left   float64
	Width  float64
	Vitals []vitalChip
	Spans  []traceSpanView
}

type traceSpanView struct {
	Span     perf.TraceSpan
	Left     float64
	Width    float64
	SelfMS   float64
	Selected bool
	Dimmed   bool
}

type traceErrTick struct {
	Err  perf.TraceError
	Left float64
}

type selfEntry struct {
	Label string
	Ms    float64
	Pct   float64
}

func (s *Server) uiTrace(w http.ResponseWriter, r *http.Request) {
	u := s.uiUser(w, r)
	if u == nil {
		return
	}
	o, p := s.uiProject(r)
	if o == nil {
		s.uiError(w, r, http.StatusNotFound, "Project not found.", nil)
		return
	}
	traceID := r.PathValue("traceID")
	if _, err := uuid.Parse(traceID); err != nil {
		s.uiError(w, r, http.StatusBadRequest, "Invalid trace id.", nil)
		return
	}
	trace, err := perf.TraceByID(r.Context(), s.pool, p.ID, traceID)
	if err != nil {
		s.uiError(w, r, 500, err.Error(), nil)
		return
	}
	if len(trace.Transactions) == 0 && len(trace.Errors) == 0 {
		s.uiError(w, r, http.StatusNotFound, "Trace not found.", nil)
		return
	}

	// Trace window: min start / max end across transactions and spans.
	var start, end time.Time
	for _, tx := range trace.Transactions {
		if start.IsZero() || tx.Start.Before(start) {
			start = tx.Start
		}
		if txEnd := tx.Start.Add(time.Duration(tx.DurationMS * float64(time.Millisecond))); end.IsZero() || txEnd.After(end) {
			end = txEnd
		}
		for _, sp := range tx.Spans {
			if !sp.Start.IsZero() && (start.IsZero() || sp.Start.Before(start)) {
				start = sp.Start
			}
			if !sp.End.IsZero() && (end.IsZero() || sp.End.After(end)) {
				end = sp.End
			}
		}
	}
	for _, e := range trace.Errors {
		if start.IsZero() || e.Timestamp.Before(start) {
			start = e.Timestamp
		}
		if end.IsZero() || e.Timestamp.After(end) {
			end = e.Timestamp
		}
	}
	total := end.Sub(start).Seconds() * 1000
	if total <= 0 {
		total = 1
	}
	place := func(t time.Time, durMS float64) (float64, float64) {
		left := t.Sub(start).Seconds() * 1000 / total * 100
		width := durMS / total * 100
		if width < 1 {
			width = 1
		}
		if left < 0 {
			left = 0
		}
		if left > 100 {
			left = 100
		}
		return left, width
	}

	opFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("op")))
	textFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	selectedSpan := r.URL.Query().Get("span")
	matched, totalSpans := 0, 0

	txViews := make([]traceTxView, 0, len(trace.Transactions))
	selfTimes := []selfEntry{}
	for _, tx := range trace.Transactions {
		left, width := place(tx.Start, tx.DurationMS)
		tv := traceTxView{Tx: tx, Left: left, Width: width}
		for name, v := range tx.Vitals {
			tv.Vitals = append(tv.Vitals, vitalChip{
				Name: strings.ToUpper(name), Value: v,
				Rating: perf.VitalRating(name, v), Unit: vitalUnit(name),
			})
		}
		sort.Slice(tv.Vitals, func(i, j int) bool { return tv.Vitals[i].Name < tv.Vitals[j].Name })

		// Self-time: span duration minus direct children's durations.
		byID := map[string]perf.TraceSpan{}
		children := map[string][]perf.TraceSpan{}
		for _, sp := range tx.Spans {
			byID[sp.SpanID] = sp
			children[sp.ParentSpanID] = append(children[sp.ParentSpanID], sp)
		}
		for _, sp := range tx.Spans {
			self := sp.DurationMS
			for _, ch := range children[sp.SpanID] {
				self -= ch.DurationMS
			}
			if self < 0 {
				self = 0
			}
			l, wd := place(sp.Start, sp.DurationMS)
			if sp.Start.IsZero() {
				l, wd = left, 0
			}
			dimmed := false
			totalSpans++
			if opFilter != "" && !strings.Contains(strings.ToLower(sp.Op), opFilter) {
				dimmed = true
			}
			if textFilter != "" && !strings.Contains(strings.ToLower(sp.Op+" "+sp.Description), textFilter) {
				dimmed = true
			}
			if !dimmed {
				matched++
			}
			tv.Spans = append(tv.Spans, traceSpanView{
				Span: sp, Left: l, Width: wd, SelfMS: self,
				Selected: selectedSpan != "" && selectedSpan == sp.SpanID,
				Dimmed:   dimmed,
			})
			selfTimes = append(selfTimes, selfEntry{Label: sp.Op + " — " + sp.Description, Ms: self})
		}
		// Root self-time: transaction duration minus top-level spans.
		rootSelf := tx.DurationMS
		for _, sp := range tx.Spans {
			if _, ok := byID[sp.ParentSpanID]; sp.ParentSpanID == "" || !ok {
				rootSelf -= sp.DurationMS
			}
		}
		if rootSelf < 0 {
			rootSelf = 0
		}
		selfTimes = append(selfTimes, selfEntry{Label: "⊤ " + tx.Name, Ms: rootSelf})
		sort.Slice(tv.Spans, func(i, j int) bool { return tv.Spans[i].Span.Start.Before(tv.Spans[j].Span.Start) })
		txViews = append(txViews, tv)
	}
	sort.Slice(selfTimes, func(i, j int) bool { return selfTimes[i].Ms > selfTimes[j].Ms })
	if len(selfTimes) > 5 {
		selfTimes = selfTimes[:5]
	}
	var selfMax float64 = 1
	for _, e := range selfTimes {
		if e.Ms > selfMax {
			selfMax = e.Ms
		}
	}
	for i := range selfTimes {
		selfTimes[i].Pct = selfTimes[i].Ms / selfMax * 100
	}

	ticks := []traceErrTick{}
	for _, e := range trace.Errors {
		l, _ := place(e.Timestamp, 0)
		ticks = append(ticks, traceErrTick{Err: e, Left: l})
	}

	var selected *perf.TraceSpan
	if selectedSpan != "" {
	outer:
		for _, tx := range trace.Transactions {
			for _, sp := range tx.Spans {
				if sp.SpanID == selectedSpan {
					cp := sp
					selected = &cp
					break outer
				}
			}
		}
	}

	web.Render(w, 200, "app_trace", web.PageData{
		Title: "Trace " + shortID(traceID),
		Data: map[string]any{
			"nav": s.uiNav(r, u, o, p, "performance"), "org": o, "project": p,
			"trace_id": traceID, "txs": txViews, "errors": ticks,
			"self": selfTimes, "self_max": selfMax,
			"total_ms": total, "op": r.URL.Query().Get("op"), "q": r.URL.Query().Get("q"),
			"matched": matched, "spans_total": totalSpans, "selected": selected,
		},
	})
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// --- releases --------------------------------------------------------------

type releaseCard struct {
	Version     string
	FirstEvent  string
	LastEvent   string
	IsNew       bool
	Total       int64
	Crashed     int64
	CrashFree   float64
	Users       uint64
	AdoptionPct float64
}

func (s *Server) uiReleases(w http.ResponseWriter, r *http.Request) {
	u := s.uiUser(w, r)
	if u == nil {
		return
	}
	o, p := s.uiProject(r)
	if o == nil {
		s.uiError(w, r, http.StatusNotFound, "Project not found.", nil)
		return
	}
	ctx := r.Context()
	rows, err := s.pool.Query(ctx, `
		SELECT version, first_event_at, last_event_at FROM releases
		WHERE project_id = $1 ORDER BY last_event_at DESC NULLS LAST, created_at DESC LIMIT 100`, p.ID)
	if err != nil {
		s.uiError(w, r, 500, err.Error(), nil)
		return
	}
	cards := []releaseCard{}
	var allTotal int64
	for rows.Next() {
		var c releaseCard
		var first, last *time.Time
		var version string
		_ = rows.Scan(&version, &first, &last)
		c.Version = version
		if first != nil {
			c.FirstEvent = first.Format(time.RFC3339)
			c.IsNew = time.Since(*first) < 24*time.Hour
		}
		if last != nil {
			c.LastEvent = last.Format(time.RFC3339)
		}
		cards = append(cards, c)
	}
	rows.Close()

	// Health over the trailing 7d (DESIGN.md §9): crash-free rates + totals.
	now := time.Now().UTC()
	health, _ := perf.ReleaseHealth(ctx, s.pool, p.ID, now.Add(-7*24*time.Hour), now)
	byRelease := map[string]perf.ReleaseHealthRow{}
	for _, h := range health {
		byRelease[h.Release] = h
		allTotal += h.Total
	}
	for i := range cards {
		if h, ok := byRelease[cards[i].Version]; ok {
			cards[i].Total, cards[i].Crashed = h.Total, h.Crashed
			cards[i].CrashFree = h.CrashFreeSessions * 100
			cards[i].Users = h.UniqueUsers
			if allTotal > 0 {
				cards[i].AdoptionPct = float64(h.Total) * 100 / float64(allTotal)
			}
		}
	}

	web.Render(w, 200, "app_releases", web.PageData{
		Title: "Releases",
		Data: map[string]any{
			"nav": s.uiNav(r, u, o, p, "releases"), "org": o, "project": p,
			"releases": cards,
		},
	})
}

// releaseIssueRow is one issue first seen in a release.
type releaseIssueRow struct {
	ID        string
	Title     string
	FirstSeen string
}

type releaseFileRow struct {
	ID         string
	Name       string
	Dist       string
	Size       int64
	DebugID    string
	Sourcemap  string
	Created    string
}

func (s *Server) uiReleaseDetail(w http.ResponseWriter, r *http.Request) {
	u := s.uiUser(w, r)
	if u == nil {
		return
	}
	o, p := s.uiProject(r)
	if o == nil {
		s.uiError(w, r, http.StatusNotFound, "Project not found.", nil)
		return
	}
	version := r.PathValue("version")
	ctx := r.Context()
	var first, last *time.Time
	var created time.Time
	if err := s.pool.QueryRow(ctx, `
		SELECT first_event_at, last_event_at, created_at FROM releases
		WHERE project_id = $1 AND version = $2`, p.ID, version).
		Scan(&first, &last, &created); err != nil {
		s.uiError(w, r, http.StatusNotFound, "Release not found.", nil)
		return
	}

	now := time.Now().UTC()
	health, _ := perf.ReleaseHealth(ctx, s.pool, p.ID, now.Add(-7*24*time.Hour), now)
	var h *perf.ReleaseHealthRow
	for i := range health {
		if health[i].Release == version {
			h = &health[i]
			break
		}
	}

	issues := []releaseIssueRow{}
	irows, err := s.pool.Query(ctx, `
		SELECT i.id::text, i.title, min(e.timestamp)::text
		FROM issues i JOIN events_part e ON e.issue_id = i.id
		WHERE i.project_id = $1 AND e.release = $2
		GROUP BY i.id, i.title ORDER BY min(e.timestamp) DESC LIMIT 20`, p.ID, version)
	if err == nil {
		defer irows.Close()
		for irows.Next() {
			var ri releaseIssueRow
			_ = irows.Scan(&ri.ID, &ri.Title, &ri.FirstSeen)
			issues = append(issues, ri)
		}
		irows.Close()
	}

	files := []releaseFileRow{}
	frows, err := s.pool.Query(ctx, `
		SELECT id::text, name, dist, size, debug_id, sourcemap_url, created_at::text
		FROM release_files WHERE project_id = $1 AND release = $2
		ORDER BY name`, p.ID, version)
	if err == nil {
		defer frows.Close()
		for frows.Next() {
			var f releaseFileRow
			_ = frows.Scan(&f.ID, &f.Name, &f.Dist, &f.Size, &f.DebugID, &f.Sourcemap, &f.Created)
			files = append(files, f)
		}
		frows.Close()
	}

	firstS, lastS := "", ""
	if first != nil {
		firstS = first.Format(time.RFC3339)
	}
	if last != nil {
		lastS = last.Format(time.RFC3339)
	}
	var crashFreePct float64
	if h != nil {
		crashFreePct = h.CrashFreeSessions * 100
	}
	web.Render(w, 200, "app_release", web.PageData{
		Title: version,
		Data: map[string]any{
			"nav": s.uiNav(r, u, o, p, "releases"), "org": o, "project": p,
			"version": version, "first_event": firstS, "last_event": lastS,
			"created": created.Format(time.RFC3339), "health": h,
			"crash_free_pct": crashFreePct,
			"is_new": first != nil && time.Since(*first) < 24*time.Hour,
			"issues": issues, "files": files,
			"can_admin": roleAtLeast(s.orgRole(r, o.Slug), "admin"),
		},
	})
}

// uiUploadReleaseFile handles the release-detail upload form (admin only):
// same caps, SHA-256 dedupe, and debug_id extraction as the sentry-cli API,
// then a redirect back to the release page.
func (s *Server) uiUploadReleaseFile(w http.ResponseWriter, r *http.Request) {
	u := s.uiUser(w, r)
	if u == nil {
		return
	}
	o, p := s.uiProject(r)
	if o == nil || !roleAtLeast(s.orgRole(r, o.Slug), "admin") {
		s.uiError(w, r, http.StatusForbidden, "Only admins can upload source maps.", nil)
		return
	}
	version := r.PathValue("version")
	var exists bool
	if err := s.pool.QueryRow(r.Context(),
		`SELECT true FROM releases WHERE project_id = $1 AND version = $2`,
		p.ID, version).Scan(&exists); err != nil {
		s.uiError(w, r, http.StatusNotFound, "Release not found.", nil)
		return
	}
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		s.uiError(w, r, http.StatusBadRequest, "Invalid upload: "+err.Error(), nil)
		return
	}
	fhs := r.MultipartForm.File["file"]
	if len(fhs) != 1 {
		s.uiError(w, r, http.StatusBadRequest, "Choose exactly one file to upload.", nil)
		return
	}
	fh := fhs[0]
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		name = fh.Filename
	}
	dist := strings.TrimSpace(r.FormValue("dist"))
	part, err := fh.Open()
	if err != nil {
		s.uiError(w, r, http.StatusBadRequest, "Unreadable file.", nil)
		return
	}
	defer part.Close()
	body, err := io.ReadAll(io.LimitReader(part, maxFileBytes+1))
	if err != nil {
		s.uiError(w, r, http.StatusBadRequest, "Read error: "+err.Error(), nil)
		return
	}
	if int64(len(body)) > maxFileBytes {
		s.uiError(w, r, http.StatusRequestEntityTooLarge, "File exceeds the 50 MB per-file limit.", nil)
		return
	}
	sum := sha256hex(body)
	var existingSHA string
	err = s.pool.QueryRow(r.Context(), `
		SELECT sha256 FROM release_files
		WHERE project_id = $1 AND release = $2 AND dist = $3 AND name = $4`,
		p.ID, version, dist, name).Scan(&existingSHA)
	if err == nil {
		if existingSHA == sum {
			http.Redirect(w, r, "/"+o.Slug+"/"+p.Slug+"/releases/"+version+"/", http.StatusFound)
			return
		}
		s.uiError(w, r, http.StatusConflict, "A file with this name but different content already exists for this release.", nil)
		return
	}
	var used int64
	_ = s.pool.QueryRow(r.Context(),
		`SELECT COALESCE(sum(size), 0) FROM release_files WHERE project_id = $1 AND release = $2`,
		p.ID, version).Scan(&used)
	if used+int64(len(body)) > maxReleaseBytes {
		s.uiError(w, r, http.StatusRequestEntityTooLarge, "Release exceeds the 500 MB storage limit.", nil)
		return
	}
	debugID, smURL := sourcemap.ExtractArtifact(name, body)
	hdrs, _ := json.Marshal(map[string]string{})
	if _, err := s.pool.Exec(r.Context(), `
		INSERT INTO release_files (id, project_id, release, dist, name, headers, body, size, sha256, debug_id, sourcemap_url)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		uuid.NewString(), p.ID, version, dist, name, hdrs, body, len(body), sum, debugID, smURL); err != nil {
		s.uiError(w, r, 500, err.Error(), nil)
		return
	}
	_ = u
	http.Redirect(w, r, "/"+o.Slug+"/"+p.Slug+"/releases/"+version+"/", http.StatusFound)
}

// --- feedback list ---------------------------------------------------------

type feedbackRow struct {
	Name, Email, Message, URL, Received, IssueID string
}

func (s *Server) uiFeedbackList(w http.ResponseWriter, r *http.Request) {
	u := s.uiUser(w, r)
	if u == nil {
		return
	}
	o, p := s.uiProject(r)
	if o == nil {
		s.uiError(w, r, http.StatusNotFound, "Project not found.", nil)
		return
	}
	rows, err := s.pool.Query(r.Context(), `
		SELECT name, contact_email, message, url, received_at::text,
		       COALESCE(issue_id::text, '')
		FROM feedbacks WHERE project_id = $1
		ORDER BY received_at DESC LIMIT 100`, p.ID)
	if err != nil {
		s.uiError(w, r, 500, err.Error(), nil)
		return
	}
	defer rows.Close()
	out := []feedbackRow{}
	for rows.Next() {
		var f feedbackRow
		_ = rows.Scan(&f.Name, &f.Email, &f.Message, &f.URL, &f.Received, &f.IssueID)
		out = append(out, f)
	}
	web.Render(w, 200, "app_feedback", web.PageData{
		Title: "User feedback",
		Data: map[string]any{
			"nav": s.uiNav(r, u, o, p, "feedback"), "org": o, "project": p,
			"feedbacks": out,
		},
	})
}
