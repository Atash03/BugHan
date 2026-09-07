package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Atash03/BugHan/internal/auth"
	"github.com/Atash03/BugHan/internal/sourcemap"
	"github.com/Atash03/BugHan/internal/web"
	"github.com/google/uuid"
)

// T8 web UI slice (DESIGN.md §13): server-rendered pages under /{org}/...
// reusing the T5 issues API store, the T6 performance/release-health queries,
// and T7 view-time symbolication. Plain HTML forms (no JS required); htmx
// progressive enhancement lands with the T9 UI polish pass.
//
// Auth: session cookie (browser). Unauthenticated page GETs redirect to the
// login page instead of 401ing like the JSON API. State-changing POSTs carry
// the session CSRF token via requireCSRF, same as the auth forms.
func (s *Server) registerWebUI(mux *http.ServeMux) {
	// A single catch-all: the /{org}/... URL family is full of wildcard
	// overlaps Go's ServeMux rejects at registration (/{org}/projects/ vs
	// /{org}/{project}/issues/, /user-style literals, ...), so uiRoute
	// dispatches on segments itself and injects them via SetPathValue.
	mux.Handle("/", http.HandlerFunc(s.uiRoute))
}

// uiRoute dispatches the /{org}/... page family. Trailing slashes are
// insignificant; unknown shapes 404 and wrong methods 405.
func (s *Server) uiRoute(w http.ResponseWriter, r *http.Request) {
	page := func(h http.HandlerFunc) http.HandlerFunc { return h }
	mutate := func(h http.HandlerFunc) http.HandlerFunc {
		return s.requireCSRF(http.HandlerFunc(h)).ServeHTTP
	}

	trimmed := strings.Trim(r.URL.Path, "/")
	var segs []string
	if trimmed != "" {
		segs = strings.Split(trimmed, "/")
	}
	get := r.Method == http.MethodGet || r.Method == http.MethodHead
	post := r.Method == http.MethodPost
	badMethod := func() {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
	set := func(kv ...string) {
		for i := 0; i+1 < len(kv); i += 2 {
			r.SetPathValue(kv[i], kv[i+1])
		}
	}

	if len(segs) == 0 {
		http.NotFound(w, r)
		return
	}
	org, rest := segs[0], segs[1:]
	set("org", org)

	// /{org}/
	if len(rest) == 0 {
		if !get {
			badMethod()
			return
		}
		page(s.uiOrgDashboard)(w, r)
		return
	}

	// Two-segment org pages: /{org}/projects, /{org}/settings.
	if len(rest) == 1 {
		switch rest[0] {
		case "projects":
			if !get {
				badMethod()
				return
			}
			page(s.uiProjects)(w, r)
			return
		case "settings":
			if !get {
				badMethod()
				return
			}
			page(s.uiOrgSettings)(w, r)
			return
		}
		// /{org}/{project}
		set("project", rest[0])
		if !get {
			badMethod()
			return
		}
		page(s.uiProjectHome)(w, r)
		return
	}

	// /{org}/projects/new
	if rest[0] == "projects" && len(rest) == 2 && rest[1] == "new" {
		if !post {
			badMethod()
			return
		}
		mutate(s.uiCreateProject)(w, r)
		return
	}

	// /{org}/settings/... (invite, invites/{id}/delete, members/{id}/remove)
	if rest[0] == "settings" {
		switch {
		case len(rest) == 2 && rest[1] == "invite":
			if !post {
				badMethod()
				return
			}
			mutate(s.uiCreateInvite)(w, r)
			return
		case len(rest) == 3 && rest[1] == "invites":
			set("inviteID", rest[2])
			if !post {
				badMethod()
				return
			}
			mutate(s.uiDeleteInvite)(w, r)
			return
		case len(rest) == 3 && rest[1] == "members":
			set("memberID", rest[2])
			if !post {
				badMethod()
				return
			}
			mutate(s.uiRemoveMember)(w, r)
			return
		}
		http.NotFound(w, r)
		return
	}

	// Everything below is /{org}/{project}/...
	set("project", rest[0])
	tail := rest[1:]
	switch {
	case len(tail) == 1 && tail[0] == "issues":
		if !get {
			badMethod()
			return
		}
		page(s.uiIssues)(w, r)
		return
	case len(tail) == 2 && tail[0] == "issues" && tail[1] == "bulk":
		if !post {
			badMethod()
			return
		}
		mutate(s.uiBulkTriage)(w, r)
		return
	case len(tail) == 2 && tail[0] == "issues":
		set("issue", tail[1])
		if !get {
			badMethod()
			return
		}
		page(s.uiIssueDetail)(w, r)
		return
	case len(tail) == 3 && tail[0] == "issues" && tail[2] == "triage":
		set("issue", tail[1])
		if !post {
			badMethod()
			return
		}
		mutate(s.uiTriageIssue)(w, r)
		return
	case len(tail) == 3 && tail[0] == "issues" && tail[2] == "comment":
		set("issue", tail[1])
		if !post {
			badMethod()
			return
		}
		mutate(s.uiCommentIssue)(w, r)
		return
	case len(tail) == 1 && tail[0] == "onboarding":
		if !get {
			badMethod()
			return
		}
		page(s.uiOnboarding)(w, r)
		return
	case len(tail) == 1 && tail[0] == "performance":
		if !get {
			badMethod()
			return
		}
		page(s.uiPerformance)(w, r)
		return
	case len(tail) == 2 && tail[0] == "traces":
		set("traceID", tail[1])
		if !get {
			badMethod()
			return
		}
		page(s.uiTrace)(w, r)
		return
	case len(tail) == 1 && tail[0] == "releases":
		if !get {
			badMethod()
			return
		}
		page(s.uiReleases)(w, r)
		return
	case len(tail) == 2 && tail[0] == "releases":
		set("version", tail[1])
		if !get {
			badMethod()
			return
		}
		page(s.uiReleaseDetail)(w, r)
		return
	case len(tail) == 3 && tail[0] == "releases" && tail[2] == "files":
		set("version", tail[1])
		if !post {
			badMethod()
			return
		}
		mutate(s.uiUploadReleaseFile)(w, r)
		return
	case len(tail) == 1 && tail[0] == "feedback":
		if !get {
			badMethod()
			return
		}
		page(s.uiFeedbackList)(w, r)
		return
	case len(tail) == 1 && tail[0] == "settings":
		if get {
			page(s.uiProjectSettings)(w, r)
			return
		}
		if post {
			mutate(s.uiUpdateProject)(w, r)
			return
		}
		badMethod()
		return
	case len(tail) == 3 && tail[0] == "settings" && tail[1] == "keys" && tail[2] == "new":
		if !post {
			badMethod()
			return
		}
		mutate(s.uiCreateKey)(w, r)
		return
	case len(tail) == 4 && tail[0] == "settings" && tail[1] == "keys" && tail[3] == "revoke":
		set("keyID", tail[2])
		if !post {
			badMethod()
			return
		}
		mutate(s.uiRevokeKey)(w, r)
		return
	}
	http.NotFound(w, r)
}

// reservedTopSegments are single-segment paths owned by the main mux
// (ingest, REST, auth pages, static assets, invite accept, org creation).
// The uiOrAPI dispatcher sends everything else to the web UI mux, so these
// segments can never be organization slugs.
func reservedTopSegments(seg string) bool {
	switch seg {
	case "", "api", "auth", "static", "accept", "orgs", "user":
		return true
	}
	return false
}

// uiUser resolves the session user or redirects anonymous browsers to login.
func (s *Server) uiUser(w http.ResponseWriter, r *http.Request) *auth.User {
	u := auth.FromContext(r.Context())
	if u == nil {
		http.Redirect(w, r, "/auth/login/", http.StatusFound)
		return nil
	}
	return u
}

// uiError renders a status-coded HTML error page.
func (s *Server) uiError(w http.ResponseWriter, r *http.Request, code int, msg string, nav map[string]any) {
	if nav == nil {
		nav = map[string]any{}
	}
	nav["csrf"] = auth.CSRF(r.Context())
	web.Render(w, code, "app_error", web.PageData{
		Title: http.StatusText(code),
		Data:  map[string]any{"nav": nav, "message": msg, "code": code},
	})
}

// uiNav builds the shared shell context: user, org list, current org and
// project, CSRF token, and the active sidebar section.
func (s *Server) uiNav(r *http.Request, u *auth.User, currentOrg, currentProject any, section string) map[string]any {
	type orgEntry struct{ Name, Slug, Role string }
	orgs := []orgEntry{}
	rows, err := s.pool.Query(r.Context(), `
		SELECT o.name, o.slug, m.role FROM organizations o
		JOIN memberships m ON m.org_id = o.id AND m.user_id = $1
		ORDER BY o.name`, u.ID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var o orgEntry
			_ = rows.Scan(&o.Name, &o.Slug, &o.Role)
			orgs = append(orgs, o)
		}
		rows.Close()
	}
	var curOrg any
	if currentOrg != nil {
		curOrg = currentOrg
	}
	var curProject any
	if currentProject != nil {
		curProject = currentProject
	}
	nav := map[string]any{
		"user_name":  u.Name,
		"user_email": u.Email,
		"orgs":       orgs,
		"org":        curOrg,
		"project":    curProject,
		"projects":   []map[string]string{},
		"csrf":       auth.CSRF(r.Context()),
		"section":    section,
	}
	if o, ok := currentOrg.(*org); ok && o != nil {
		nav["projects"] = s.navProjects(r, o.ID)
	}
	return nav
}

func (s *Server) navProjects(r *http.Request, orgID string) []map[string]string {
	rows, err := s.pool.Query(r.Context(),
		`SELECT name, slug FROM projects WHERE org_id = $1 ORDER BY name`, orgID)
	if err != nil {
		return []map[string]string{}
	}
	defer rows.Close()
	out := []map[string]string{}
	for rows.Next() {
		var name, slug string
		_ = rows.Scan(&name, &slug)
		out = append(out, map[string]string{"name": name, "slug": slug})
	}
	return out
}

// uiOrg resolves {org} for a page: 404 (no existence leak) when missing or
// when the viewer is not a member. Reserved top-level segments 404 too.
func (s *Server) uiOrg(r *http.Request) *org {
	slug := r.PathValue("org")
	if reservedTopSegments(slug) {
		return nil
	}
	return s.orgFromPath(r)
}

// uiProject resolves {org}/{project} for a page with a membership check.
func (s *Server) uiProject(r *http.Request) (*org, *project) {
	o := s.uiOrg(r)
	if o == nil {
		return nil, nil
	}
	p := s.projectFromPath(r)
	if p == nil || p.OrgID != o.ID {
		return nil, nil
	}
	if role := s.orgRole(r, o.Slug); !roleAtLeast(role, "member") {
		return nil, nil
	}
	return o, p
}

// uiIssue resolves an issue page's org/project/issue triple.
func (s *Server) uiIssue(r *http.Request) (*org, *project, *issueRef) {
	o, p := s.uiProject(r)
	if o == nil {
		return nil, nil, nil
	}
	ref := s.issueFromPath(r)
	if ref == nil || ref.ProjectID != p.ID {
		return nil, nil, nil
	}
	return o, p, ref
}

// --- org dashboard ---------------------------------------------------------

func (s *Server) uiOrgDashboard(w http.ResponseWriter, r *http.Request) {
	u := s.uiUser(w, r)
	if u == nil {
		return
	}
	o := s.uiOrg(r)
	if o == nil {
		s.uiError(w, r, http.StatusNotFound, "Organization not found.", nil)
		return
	}
	if !roleAtLeast(s.orgRole(r, o.Slug), "member") {
		s.uiError(w, r, http.StatusNotFound, "Organization not found.", nil)
		return
	}
	ctx := r.Context()
	nav := s.uiNav(r, u, o, nil, "dashboard")

	var unresolved, projects int
	var events24h int64
	_ = s.pool.QueryRow(ctx, `
		SELECT count(*) FROM issues i JOIN projects p ON p.id = i.project_id
		WHERE p.org_id = $1 AND i.status = 'unresolved'`, o.ID).Scan(&unresolved)
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM projects WHERE org_id = $1`, o.ID).Scan(&projects)
	_ = s.pool.QueryRow(ctx, `
		SELECT COALESCE(sum(per.count), 0) FROM project_event_rollups per
		JOIN projects p ON p.id = per.project_id
		WHERE p.org_id = $1 AND per.hour > now() - interval '24 hours'`, o.ID).Scan(&events24h)

	// Events-over-time: hourly org totals for the last 24h (rollups persist
	// past raw retention and are cheap to scan).
	type bucket struct {
		Hour  string
		Count int64
	}
	chart := []bucket{}
	rows, err := s.pool.Query(ctx, `
		SELECT date_trunc('hour', per.hour)::text, sum(per.count) FROM project_event_rollups per
		JOIN projects p ON p.id = per.project_id
		WHERE p.org_id = $1 AND per.hour > now() - interval '24 hours'
		GROUP BY 1 ORDER BY 1`, o.ID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var b bucket
			_ = rows.Scan(&b.Hour, &b.Count)
			chart = append(chart, b)
		}
		rows.Close()
	}
	var chartMax int64 = 1
	for _, b := range chart {
		if b.Count > chartMax {
			chartMax = b.Count
		}
	}

	// Latest issues across the org's projects.
	latest := s.queryIssueCards(ctx, o.ID, 8)

	// Release health strip: newest releases with last-event timestamps.
	type release struct{ Project, Slug, Version, LastEvent string }
	rels := []release{}
	rrows, err := s.pool.Query(ctx, `
		SELECT p.name, p.slug, r.version, COALESCE(r.last_event_at::text, '')
		FROM releases r JOIN projects p ON p.id = r.project_id
		WHERE p.org_id = $1 ORDER BY r.last_event_at DESC NULLS LAST LIMIT 5`, o.ID)
	if err == nil {
		defer rrows.Close()
		for rrows.Next() {
			var rel release
			_ = rrows.Scan(&rel.Project, &rel.Slug, &rel.Version, &rel.LastEvent)
			rels = append(rels, rel)
		}
		rrows.Close()
	}

	web.Render(w, 200, "app_dashboard", web.PageData{
		Title: o.Name,
		Data: map[string]any{
			"nav": nav, "org": o,
			"unresolved": unresolved, "projects": projects, "events24h": events24h,
			"chart": chart, "chart_max": chartMax,
			"latest": latest, "releases": rels,
		},
	})
}

// issueCard is the shared row shape for issue lists and the dashboard.
type issueCard struct {
	ID, Title, Culprit, Type, Level, Status, Substatus string
	Count                                              int64
	UserCount                                          int
	LastSeen, FirstSeen                                string
	ProjectSlug, ProjectName                           string
	AssigneeName                                       string
	Spark                                              []int64
	SparkMax                                           int64
}

func (s *Server) queryIssueCards(ctx context.Context, orgID string, limit int) []issueCard {
	rows, err := s.pool.Query(ctx, `
		SELECT `+issueSelectCols+`, p.slug, p.name
		FROM issues i
		LEFT JOIN users u ON u.id = i.assignee_id
		JOIN projects p ON p.id = i.project_id
		WHERE p.org_id = $1
		ORDER BY i.last_seen DESC LIMIT $2`, orgID, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []issueCard{}
	for rows.Next() {
		var c issueCard
		var id, fingerprint, title, culprit, typ, level, status, substatus string
		var firstSeen, lastSeen string
		var assigneeID, assigneeEmail, assigneeName string
		if err := rows.Scan(&id, &fingerprint, &title, &culprit, &typ, &level,
			&status, &substatus, &firstSeen, &lastSeen, &c.Count, &c.UserCount,
			&assigneeID, &assigneeEmail, &assigneeName,
			&c.ProjectSlug, &c.ProjectName); err != nil {
			continue
		}
		c.ID, c.Title, c.Culprit, c.Type, c.Level = id, title, culprit, typ, level
		c.Status, c.Substatus = status, substatus
		c.FirstSeen, c.LastSeen = firstSeen, lastSeen
		c.AssigneeName = assigneeName
		if c.AssigneeName == "" {
			c.AssigneeName = assigneeEmail
		}
		out = append(out, c)
	}
	return out
}

func (s *Server) uiProjects(w http.ResponseWriter, r *http.Request) {
	u := s.uiUser(w, r)
	if u == nil {
		return
	}
	o := s.uiOrg(r)
	if o == nil || !roleAtLeast(s.orgRole(r, o.Slug), "member") {
		s.uiError(w, r, http.StatusNotFound, "Organization not found.", nil)
		return
	}
	type card struct {
		Name, Slug, Platform, ID, DSN string
		Issues                        int
		Events24h                     int64
	}
	cards := []card{}
	rows, err := s.pool.Query(r.Context(), `
		SELECT p.id::text, p.name, p.slug, p.platform,
			(SELECT count(*) FROM issues i WHERE i.project_id = p.id AND i.status = 'unresolved'),
			(SELECT COALESCE(sum(per.count), 0) FROM project_event_rollups per
			 WHERE per.project_id = p.id AND per.hour > now() - interval '24 hours')
		FROM projects p WHERE p.org_id = $1 ORDER BY p.name`, o.ID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var c card
			_ = rows.Scan(&c.ID, &c.Name, &c.Slug, &c.Platform, &c.Issues, &c.Events24h)
			var keyID string
			_ = s.pool.QueryRow(r.Context(), `
				SELECT id::text FROM ingest_keys WHERE project_id = $1 AND revoked_at IS NULL
				ORDER BY created_at LIMIT 1`, c.ID).Scan(&keyID)
			if keyID != "" {
				c.DSN = s.dsnFor(&project{ID: c.ID}, keyID)
			}
			cards = append(cards, c)
		}
		rows.Close()
	}
	web.Render(w, 200, "app_projects", web.PageData{
		Title: "Projects",
		Data: map[string]any{
			"nav": s.uiNav(r, u, o, nil, "projects"), "org": o,
			"projects": cards, "can_admin": roleAtLeast(s.orgRole(r, o.Slug), "admin"),
		},
	})
}

func (s *Server) uiCreateProject(w http.ResponseWriter, r *http.Request) {
	u := s.uiUser(w, r)
	if u == nil {
		return
	}
	o := s.uiOrg(r)
	if o == nil || !roleAtLeast(s.orgRole(r, o.Slug), "admin") {
		s.uiError(w, r, http.StatusForbidden, "Only admins can create projects.", nil)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	platform := strings.TrimSpace(r.PostFormValue("platform"))
	if platform == "" {
		platform = "javascript"
	}
	if name == "" {
		http.Redirect(w, r, "/"+o.Slug+"/projects/", http.StatusFound)
		return
	}
	id := uuid.NewString()
	slug := auth.Slugify(name)
	var finalSlug string
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.uiError(w, r, 500, err.Error(), nil)
		return
	}
	defer tx.Rollback(r.Context())
	if err := tx.QueryRow(r.Context(), `
		INSERT INTO projects (id, org_id, name, slug, platform)
		VALUES ($1, $2, $3, $4, $5) RETURNING slug`,
		id, o.ID, name, slug, platform).Scan(&finalSlug); err != nil {
		s.uiError(w, r, 400, "Could not create project: "+err.Error(), nil)
		return
	}
	if _, err := tx.Exec(r.Context(),
		`INSERT INTO ingest_keys (id, project_id, name) VALUES ($1, $2, 'Default')`,
		uuid.NewString(), id); err != nil {
		s.uiError(w, r, 500, err.Error(), nil)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.uiError(w, r, 500, err.Error(), nil)
		return
	}
	_ = u
	http.Redirect(w, r, "/"+o.Slug+"/"+finalSlug+"/onboarding/", http.StatusFound)
}

func (s *Server) uiProjectHome(w http.ResponseWriter, r *http.Request) {
	u := s.uiUser(w, r)
	if u == nil {
		return
	}
	o, p := s.uiProject(r)
	if o == nil {
		s.uiError(w, r, http.StatusNotFound, "Project not found.", nil)
		return
	}
	_ = u
	http.Redirect(w, r, "/"+o.Slug+"/"+p.Slug+"/issues/", http.StatusFound)
}

// --- issues list -----------------------------------------------------------

func (s *Server) uiIssues(w http.ResponseWriter, r *http.Request) {
	u := s.uiUser(w, r)
	if u == nil {
		return
	}
	o, p := s.uiProject(r)
	if o == nil {
		s.uiError(w, r, http.StatusNotFound, "Project not found.", nil)
		return
	}
	q := r.URL.Query()
	status := q.Get("status")
	if status == "" {
		status = "unresolved"
	}
	if status != "unresolved" && status != "resolved" && status != "ignored" && status != "all" {
		status = "unresolved"
	}
	search := strings.TrimSpace(q.Get("q"))
	sortCol, ok := issueSortCols[q.Get("sort")]
	if !ok {
		sortCol = issueSortCols["last_seen"]
	}
	env := strings.TrimSpace(q.Get("environment"))

	args := []any{}
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	conds := []string{"i.project_id = " + arg(p.ID)}
	if status != "all" {
		conds = append(conds, "i.status = "+arg(status))
	}
	if search != "" {
		pat := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(search) + "%"
		conds = append(conds, "(i.title ILIKE "+arg(pat)+" OR i.culprit ILIKE "+arg(pat)+")")
	}
	if env != "" {
		conds = append(conds, "EXISTS (SELECT 1 FROM events_part e WHERE e.issue_id = i.id AND e.environment = "+arg(env)+")")
	}
	where := "WHERE " + strings.Join(conds, " AND ")

	rows, err := s.pool.Query(r.Context(), `
		SELECT `+issueSelectCols+`
		FROM issues i LEFT JOIN users u ON u.id = i.assignee_id
		`+where+` ORDER BY `+sortCol+` DESC LIMIT 100`, args...)
	if err != nil {
		s.uiError(w, r, 500, err.Error(), nil)
		return
	}
	defer rows.Close()
	cards := []issueCard{}
	for rows.Next() {
		m := scanIssueRow(rows)
		c := issueCardFromMap(m)
		c.Spark, c.SparkMax = s.issueSpark(r, m["id"].(string))
		cards = append(cards, c)
	}
	rows.Close()

	envs := s.projectEnvironments(r, p.ID)
	saved := s.projectSavedViews(r, p.ID)

	web.Render(w, 200, "app_issues", web.PageData{
		Title: "Issues",
		Data: map[string]any{
			"nav": s.uiNav(r, u, o, p, "issues"), "org": o, "project": p,
			"issues": cards, "status": status, "q": search, "sort": q.Get("sort"),
			"environment": env, "envs": envs, "views": saved,
			"can_triage": roleAtLeast(s.orgRole(r, o.Slug), "member"),
		},
	})
}

func issueCardFromMap(m map[string]any) issueCard {
	str := func(k string) string {
		if v, ok := m[k].(string); ok {
			return v
		}
		return ""
	}
	var count int64
	switch v := m["count"].(type) {
	case int64:
		count = v
	case int:
		count = int64(v)
	case int32:
		count = int64(v)
	}
	var users int
	switch v := m["user_count"].(type) {
	case int:
		users = v
	case int64:
		users = int(v)
	case int32:
		users = int(v)
	}
	name := ""
	if a, ok := m["assignee"].(map[string]string); ok {
		name = a["name"]
		if name == "" {
			name = a["email"]
		}
	}
	return issueCard{
		ID: m["id"].(string), Title: str("title"), Culprit: str("culprit"),
		Type: str("type"), Level: str("level"), Status: str("status"), Substatus: str("substatus"),
		Count: count, UserCount: users,
		LastSeen: str("last_seen"), FirstSeen: str("first_seen"), AssigneeName: name,
	}
}

// issueSpark loads the last 24 hourly buckets for an issue's sparkline.
func (s *Server) issueSpark(r *http.Request, issueID string) ([]int64, int64) {
	rows, err := s.pool.Query(r.Context(), `
		SELECT count FROM event_rollups WHERE issue_id = $1
		AND hour > now() - interval '24 hours' ORDER BY hour`, issueID)
	if err != nil {
		return nil, 1
	}
	defer rows.Close()
	out := []int64{}
	var max int64 = 1
	for rows.Next() {
		var c int64
		_ = rows.Scan(&c)
		out = append(out, c)
		if c > max {
			max = c
		}
	}
	return out, max
}

func (s *Server) projectEnvironments(r *http.Request, projectID string) []string {
	rows, err := s.pool.Query(r.Context(),
		`SELECT DISTINCT environment FROM events_part WHERE project_id = $1 ORDER BY 1 LIMIT 20`, projectID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var e string
		_ = rows.Scan(&e)
		out = append(out, e)
	}
	return out
}

func (s *Server) projectSavedViews(r *http.Request, projectID string) []map[string]any {
	rows, err := s.pool.Query(r.Context(),
		`SELECT name, query FROM saved_views WHERE project_id = $1 ORDER BY created_at`, projectID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var name, query string
		_ = rows.Scan(&name, &query)
		out = append(out, map[string]any{"name": name, "query": query})
	}
	return out
}

func (s *Server) uiBulkTriage(w http.ResponseWriter, r *http.Request) {
	u := s.uiUser(w, r)
	if u == nil {
		return
	}
	o, p := s.uiProject(r)
	if o == nil {
		s.uiError(w, r, http.StatusNotFound, "Project not found.", nil)
		return
	}
	_ = u
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/"+o.Slug+"/"+p.Slug+"/issues/", http.StatusFound)
		return
	}
	ids := r.PostForm["ids"]
	action := r.PostFormValue("action")
	if len(ids) == 0 || (action != "resolved" && action != "ignored" && action != "unresolved") {
		http.Redirect(w, r, "/"+o.Slug+"/"+p.Slug+"/issues/", http.StatusFound)
		return
	}
	valid := []string{}
	for _, id := range ids {
		if _, err := uuid.Parse(id); err == nil {
			valid = append(valid, id)
		}
	}
	if len(valid) > 0 {
		actor := ""
		if me := auth.FromContext(r.Context()); me != nil {
			actor = me.ID
		}
		_, _ = s.pool.Exec(r.Context(),
			`UPDATE issues SET status = $1, substatus = '' WHERE id = ANY($2::uuid[]) AND project_id = $3`,
			action, valid, p.ID)
		_, _ = s.pool.Exec(r.Context(), `
			INSERT INTO issue_activity (issue_id, author_id, type, data)
			SELECT id, NULLIF($1, '')::uuid, 'status', jsonb_build_object('to', $2::text)
			FROM issues WHERE id = ANY($3::uuid[]) AND project_id = $4`,
			actor, action, valid, p.ID)
	}
	http.Redirect(w, r, "/"+o.Slug+"/"+p.Slug+"/issues/", http.StatusFound)
}

// --- issue detail ----------------------------------------------------------

func (s *Server) uiIssueDetail(w http.ResponseWriter, r *http.Request) {
	u := s.uiUser(w, r)
	if u == nil {
		return
	}
	o, p, ref := s.uiIssue(r)
	if o == nil {
		s.uiError(w, r, http.StatusNotFound, "Issue not found.", nil)
		return
	}
	ctx := r.Context()
	var m map[string]any
	row := s.pool.QueryRow(ctx, `
		SELECT `+issueSelectCols+`
		FROM issues i LEFT JOIN users u ON u.id = i.assignee_id
		WHERE i.id = $1`, ref.ID)
	m = scanIssueRow(row)
	if _, ok := m["id"]; !ok {
		s.uiError(w, r, http.StatusNotFound, "Issue not found.", nil)
		return
	}
	card := issueCardFromMap(m)
	card.ProjectSlug, card.ProjectName = p.Slug, p.Name

	// Event selector: ?event=<uuid> or the latest event.
	eventID := r.URL.Query().Get("event")
	var evID uuid.UUID
	var evTS time.Time
	var payload []byte
	if eventID != "" {
		if id, err := uuid.Parse(eventID); err == nil {
			evID = id
			_ = s.pool.QueryRow(ctx, `
				SELECT timestamp, payload FROM events_part WHERE id = $1 AND issue_id = $2`,
				evID, ref.ID).Scan(&evTS, &payload)
		}
	}
	if len(payload) == 0 {
		_ = s.pool.QueryRow(ctx, `
			SELECT id, timestamp, payload FROM events_part WHERE issue_id = $1
			ORDER BY timestamp DESC LIMIT 1`, ref.ID).Scan(&evID, &evTS, &payload)
	}

	var parsed struct {
		Title       string            `json:"title"`
		Environment string            `json:"environment"`
		Release     string            `json:"release"`
		Dist        string            `json:"dist"`
		Level       string            `json:"level"`
		Tags        map[string]string `json:"tags"`
		Breadcrumbs any               `json:"breadcrumbs"`
		Contexts    map[string]any    `json:"contexts"`
		Trace       struct {
			TraceID string `json:"trace_id"`
			SpanID  string `json:"span_id"`
		} `json:"trace"`
		ContextsRaw map[string]any `json:"-"`
		Exception   struct {
			Values []map[string]any `json:"values"`
		} `json:"exception"`
		Request any `json:"request"`
		User    any `json:"user"`
	}
	eventMeta := map[string]any{}
	if len(payload) > 0 {
		_ = json.Unmarshal(payload, &parsed)
		// trace_id/span_id live under contexts.trace in SDK payloads.
		if tr, ok := parsed.Contexts["trace"].(map[string]any); ok {
			if v, ok := tr["trace_id"].(string); ok {
				parsed.Trace.TraceID = v
			}
			if v, ok := tr["span_id"].(string); ok {
				parsed.Trace.SpanID = v
			}
		}
		var ts string
		var traceID, spanID string
		_ = s.pool.QueryRow(ctx, `
			SELECT timestamp::text, COALESCE(trace_id::text, ''), COALESCE(span_id, '')
			FROM events_part WHERE id = $1`, evID).Scan(&ts, &traceID, &spanID)
		eventMeta = map[string]any{
			"id": evID.String(), "timestamp": ts,
			"trace_id": traceID, "span_id": spanID,
		}
	}

	// View-time symbolication (T7): best-effort, cached on the event row.
	var sym *sourcemap.EventResult
	if len(payload) > 0 {
		sym, _ = sourcemap.SymbolicateEvent(ctx, s.pool, p.ID, evID, evTS, payload)
	}

	// Recent events for the pager/list.
	type evRow struct{ ID, TS, Title string }
	evs := []evRow{}
	erows, err := s.pool.Query(ctx, `
		SELECT id::text, timestamp::text, title FROM events_part
		WHERE issue_id = $1 ORDER BY timestamp DESC LIMIT 20`, ref.ID)
	if err == nil {
		defer erows.Close()
		for erows.Next() {
			var e evRow
			_ = erows.Scan(&e.ID, &e.TS, &e.Title)
			evs = append(evs, e)
		}
		erows.Close()
	}

	// Activity feed + members (for the assign box) + feedbacks.
	feed := []map[string]any{}
	frows, err := s.pool.Query(ctx, `
		SELECT a.type, a.body, a.data, a.created_at::text,
		       COALESCE(u.name, ''), COALESCE(u.email, '')
		FROM issue_activity a LEFT JOIN users u ON u.id = a.author_id
		WHERE a.issue_id = $1 ORDER BY a.created_at DESC, a.id DESC LIMIT 50`, ref.ID)
	if err == nil {
		defer frows.Close()
		for frows.Next() {
			var typ, body, created, name, email string
			var data map[string]any
			_ = frows.Scan(&typ, &body, &data, &created, &name, &email)
			who := name
			if who == "" {
				who = email
			}
			feed = append(feed, map[string]any{
				"type": typ, "body": body, "data": data, "created": created, "author": who,
			})
		}
		frows.Close()
	}
	members := []map[string]string{}
	mrows, err := s.pool.Query(ctx, `
		SELECT u.id::text, u.name, u.email FROM users u
		JOIN memberships m ON m.user_id = u.id WHERE m.org_id = $1 ORDER BY u.name`, o.ID)
	if err == nil {
		defer mrows.Close()
		for mrows.Next() {
			var id, name, email string
			_ = mrows.Scan(&id, &name, &email)
			members = append(members, map[string]string{"id": id, "name": name, "email": email})
		}
		mrows.Close()
	}
	type fb struct{ Name, Email, Message, TS string }
	fbs := []fb{}
	brows, err := s.pool.Query(ctx, `
		SELECT name, contact_email, message, received_at::text FROM feedbacks
		WHERE issue_id = $1 ORDER BY received_at DESC LIMIT 20`, ref.ID)
	if err == nil {
		defer brows.Close()
		for brows.Next() {
			var f fb
			_ = brows.Scan(&f.Name, &f.Email, &f.Message, &f.TS)
			fbs = append(fbs, f)
		}
		brows.Close()
	}

	web.Render(w, 200, "app_issue", web.PageData{
		Title: card.Title,
		Data: map[string]any{
			"nav": s.uiNav(r, u, o, p, "issues"), "org": o, "project": p,
			"issue": card, "event": eventMeta,
			"env": parsed.Environment, "release": parsed.Release, "dist": parsed.Dist,
			"level": parsed.Level, "tags": parsed.Tags,
			"breadcrumbs": parsed.Breadcrumbs, "contexts": parsed.Contexts,
			"exceptions": parsed.Exception.Values,
			"request":    parsed.Request, "event_user": parsed.User,
			"trace_id": parsed.Trace.TraceID, "span_id": parsed.Trace.SpanID,
			"symbolicated": sym, "events": evs,
			"feed": feed, "members": members, "feedbacks": fbs,
		},
	})
}

func (s *Server) uiTriageIssue(w http.ResponseWriter, r *http.Request) {
	u := s.uiUser(w, r)
	if u == nil {
		return
	}
	o, p, ref := s.uiIssue(r)
	if o == nil {
		s.uiError(w, r, http.StatusNotFound, "Issue not found.", nil)
		return
	}
	_ = u
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, issueURL(o, p, ref.ID), http.StatusFound)
		return
	}
	actor := ""
	if me := auth.FromContext(r.Context()); me != nil {
		actor = me.ID
	}
	if status := r.PostFormValue("status"); status == "unresolved" || status == "resolved" || status == "ignored" {
		var from string
		_ = s.pool.QueryRow(r.Context(), `SELECT status FROM issues WHERE id = $1`, ref.ID).Scan(&from)
		_, _ = s.pool.Exec(r.Context(),
			`UPDATE issues SET status = $1, substatus = '' WHERE id = $2`, status, ref.ID)
		if from != "" && from != status {
			s.recordActivity(r, ref.ID, actor, "status", "",
				map[string]string{"from": from, "to": status})
		}
	}
	if assignee, ok := r.PostForm["assignedTo"]; ok {
		to := ""
		if len(assignee) > 0 {
			to = strings.TrimSpace(assignee[0])
		}
		if to == "" || to == "unassign" {
			_, _ = s.pool.Exec(r.Context(), `UPDATE issues SET assignee_id = NULL WHERE id = $1`, ref.ID)
			s.recordActivity(r, ref.ID, actor, "assignment", "", map[string]any{"assignee": nil})
		} else {
			if id, err := s.resolveAssignee(r, ref, to); err == nil {
				_, _ = s.pool.Exec(r.Context(), `UPDATE issues SET assignee_id = $1 WHERE id = $2`, id, ref.ID)
				s.recordActivity(r, ref.ID, actor, "assignment", "", map[string]any{"assignee": id})
			}
		}
	}
	http.Redirect(w, r, issueURL(o, p, ref.ID), http.StatusFound)
}

func (s *Server) uiCommentIssue(w http.ResponseWriter, r *http.Request) {
	u := s.uiUser(w, r)
	if u == nil {
		return
	}
	o, p, ref := s.uiIssue(r)
	if o == nil {
		s.uiError(w, r, http.StatusNotFound, "Issue not found.", nil)
		return
	}
	_ = u
	if err := r.ParseForm(); err == nil {
		if body := strings.TrimSpace(r.PostFormValue("body")); body != "" {
			actor := ""
			if me := auth.FromContext(r.Context()); me != nil {
				actor = me.ID
			}
			s.recordActivity(r, ref.ID, actor, "note", body, map[string]any{})
		}
	}
	http.Redirect(w, r, issueURL(o, p, ref.ID), http.StatusFound)
}

func issueURL(o *org, p *project, issueID string) string {
	return "/" + o.Slug + "/" + p.Slug + "/issues/" + issueID + "/"
}

// --- onboarding --------------------------------------------------------

func (s *Server) uiOnboarding(w http.ResponseWriter, r *http.Request) {
	u := s.uiUser(w, r)
	if u == nil {
		return
	}
	o, p := s.uiProject(r)
	if o == nil {
		s.uiError(w, r, http.StatusNotFound, "Project not found.", nil)
		return
	}
	var keyID string
	_ = s.pool.QueryRow(r.Context(), `
		SELECT id::text FROM ingest_keys WHERE project_id = $1 AND revoked_at IS NULL
		ORDER BY created_at LIMIT 1`, p.ID).Scan(&keyID)
	dsn := ""
	if keyID != "" {
		dsn = s.dsnFor(p, keyID)
	}
	web.Render(w, 200, "app_onboarding", web.PageData{
		Title: "Get started",
		Data: map[string]any{
			"nav": s.uiNav(r, u, o, p, "projects"), "org": o, "project": p,
			"dsn": dsn,
		},
	})
}

// --- settings ------------------------------------------------------------

func (s *Server) uiOrgSettings(w http.ResponseWriter, r *http.Request) {
	u := s.uiUser(w, r)
	if u == nil {
		return
	}
	o := s.uiOrg(r)
	if o == nil || !roleAtLeast(s.orgRole(r, o.Slug), "member") {
		s.uiError(w, r, http.StatusNotFound, "Organization not found.", nil)
		return
	}
	role := s.orgRole(r, o.Slug)
	type member struct{ ID, UserID, Email, Name, Role, Joined string }
	members := []member{}
	mrows, err := s.pool.Query(r.Context(), `
		SELECT m.id::text, u.id::text, u.email, u.name, m.role, m.created_at::text
		FROM memberships m JOIN users u ON u.id = m.user_id
		WHERE m.org_id = $1 ORDER BY m.created_at`, o.ID)
	if err == nil {
		defer mrows.Close()
		for mrows.Next() {
			var m member
			_ = mrows.Scan(&m.ID, &m.UserID, &m.Email, &m.Name, &m.Role, &m.Joined)
			members = append(members, m)
		}
		mrows.Close()
	}
	type invite struct{ ID, Email, Role, Expires, Accepted string }
	invites := []invite{}
	if roleAtLeast(role, "admin") {
		irows, err := s.pool.Query(r.Context(), `
			SELECT id::text, email, role, expires_at::text, COALESCE(accepted_at::text, '')
			FROM invitations WHERE org_id = $1 AND expires_at > now() - interval '30 days'
			ORDER BY created_at DESC`, o.ID)
		if err == nil {
			defer irows.Close()
			for irows.Next() {
				var in invite
				_ = irows.Scan(&in.ID, &in.Email, &in.Role, &in.Expires, &in.Accepted)
				invites = append(invites, in)
			}
			irows.Close()
		}
	}
	web.Render(w, 200, "app_org_settings", web.PageData{
		Title: "Organization settings",
		Data: map[string]any{
			"nav": s.uiNav(r, u, o, nil, "settings"), "org": o,
			"members": members, "invites": invites,
			"can_admin": roleAtLeast(role, "admin"), "my_id": u.ID,
		},
	})
}

func (s *Server) uiCreateInvite(w http.ResponseWriter, r *http.Request) {
	u := s.uiUser(w, r)
	if u == nil {
		return
	}
	o := s.uiOrg(r)
	if o == nil || !roleAtLeast(s.orgRole(r, o.Slug), "admin") {
		s.uiError(w, r, http.StatusForbidden, "Only admins can invite.", nil)
		return
	}
	_ = u
	if err := r.ParseForm(); err == nil {
		email := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))
		role := r.PostFormValue("role")
		if role != "admin" {
			role = "member"
		}
		if email != "" {
			raw, _ := auth.NewToken()
			if _, err := s.pool.Exec(r.Context(), `
				INSERT INTO invitations (id, org_id, email, role, token_hash, invited_by, expires_at)
				VALUES ($1, $2, $3, $4, $5, $6, now() + interval '7 days')`,
				uuid.NewString(), o.ID, email, role, auth.HashToken(raw), u.ID); err == nil {
				link := s.cfg.PublicURL + "/accept/" + raw
				_ = s.mailer.Send(email, "You're invited to "+o.Name+" on BugHan",
					"Accept your invitation:\n"+link+"\n\nThe link expires in 7 days.")
			}
		}
	}
	http.Redirect(w, r, "/"+o.Slug+"/settings/", http.StatusFound)
}

func (s *Server) uiDeleteInvite(w http.ResponseWriter, r *http.Request) {
	if s.uiUser(w, r) == nil {
		return
	}
	o := s.uiOrg(r)
	if o == nil || !roleAtLeast(s.orgRole(r, o.Slug), "admin") {
		s.uiError(w, r, http.StatusForbidden, "Only admins can manage invites.", nil)
		return
	}
	_, _ = s.pool.Exec(r.Context(),
		`DELETE FROM invitations WHERE id = $1 AND org_id = $2 AND accepted_at IS NULL`,
		r.PathValue("inviteID"), o.ID)
	http.Redirect(w, r, "/"+o.Slug+"/settings/", http.StatusFound)
}

func (s *Server) uiRemoveMember(w http.ResponseWriter, r *http.Request) {
	if s.uiUser(w, r) == nil {
		return
	}
	o := s.uiOrg(r)
	if o == nil || !roleAtLeast(s.orgRole(r, o.Slug), "admin") {
		s.uiError(w, r, http.StatusForbidden, "Only admins can manage members.", nil)
		return
	}
	memberID := r.PathValue("memberID")
	var count int
	_ = s.pool.QueryRow(r.Context(),
		`SELECT count(*) FROM memberships WHERE org_id = $1 AND role = 'owner'`, o.ID).Scan(&count)
	var role, uid string
	if err := s.pool.QueryRow(r.Context(),
		`SELECT role, user_id::text FROM memberships WHERE id = $1 AND org_id = $2`,
		memberID, o.ID).Scan(&role, &uid); err == nil {
		if !(role == "owner" && count <= 1) {
			_, _ = s.pool.Exec(r.Context(), `DELETE FROM memberships WHERE id = $1`, memberID)
			_, _ = s.pool.Exec(r.Context(), `DELETE FROM auth_sessions WHERE user_id = $1`, uid)
		}
	}
	http.Redirect(w, r, "/"+o.Slug+"/settings/", http.StatusFound)
}

func (s *Server) uiProjectSettings(w http.ResponseWriter, r *http.Request) {
	u := s.uiUser(w, r)
	if u == nil {
		return
	}
	o, p := s.uiProject(r)
	if o == nil {
		s.uiError(w, r, http.StatusNotFound, "Project not found.", nil)
		return
	}
	type key struct {
		ID, Name, Created, DSN string
		Active                 bool
	}
	keys := []key{}
	krows, err := s.pool.Query(r.Context(), `
		SELECT id::text, name, created_at::text, (revoked_at IS NULL)
		FROM ingest_keys WHERE project_id = $1 ORDER BY created_at`, p.ID)
	if err == nil {
		defer krows.Close()
		for krows.Next() {
			var k key
			_ = krows.Scan(&k.ID, &k.Name, &k.Created, &k.Active)
			if k.Active {
				k.DSN = s.dsnFor(p, k.ID)
			}
			keys = append(keys, k)
		}
		krows.Close()
	}
	web.Render(w, 200, "app_project_settings", web.PageData{
		Title: "Project settings",
		Data: map[string]any{
			"nav": s.uiNav(r, u, o, p, "settings"), "org": o, "project": p,
			"keys": keys, "can_admin": roleAtLeast(s.orgRole(r, o.Slug), "admin"),
		},
	})
}

func (s *Server) uiUpdateProject(w http.ResponseWriter, r *http.Request) {
	if s.uiUser(w, r) == nil {
		return
	}
	o, p := s.uiProject(r)
	if o == nil || !roleAtLeast(s.orgRole(r, o.Slug), "admin") {
		s.uiError(w, r, http.StatusForbidden, "Only admins can edit project settings.", nil)
		return
	}
	if err := r.ParseForm(); err == nil {
		name := strings.TrimSpace(r.PostFormValue("name"))
		platform := strings.TrimSpace(r.PostFormValue("platform"))
		if name != "" {
			_, _ = s.pool.Exec(r.Context(),
				`UPDATE projects SET name = $1 WHERE id = $2`, name, p.ID)
		}
		if platform != "" {
			_, _ = s.pool.Exec(r.Context(),
				`UPDATE projects SET platform = $1 WHERE id = $2`, platform, p.ID)
		}
	}
	http.Redirect(w, r, "/"+o.Slug+"/"+p.Slug+"/settings/", http.StatusFound)
}

func (s *Server) uiCreateKey(w http.ResponseWriter, r *http.Request) {
	if s.uiUser(w, r) == nil {
		return
	}
	o, p := s.uiProject(r)
	if o == nil || !roleAtLeast(s.orgRole(r, o.Slug), "admin") {
		s.uiError(w, r, http.StatusForbidden, "Only admins can manage keys.", nil)
		return
	}
	if err := r.ParseForm(); err == nil {
		name := strings.TrimSpace(r.PostFormValue("name"))
		if name == "" {
			name = "Key " + timeNowShort()
		}
		_, _ = s.pool.Exec(r.Context(),
			`INSERT INTO ingest_keys (id, project_id, name) VALUES ($1, $2, $3)`,
			uuid.NewString(), p.ID, name)
	}
	http.Redirect(w, r, "/"+o.Slug+"/"+p.Slug+"/settings/", http.StatusFound)
}

func (s *Server) uiRevokeKey(w http.ResponseWriter, r *http.Request) {
	if s.uiUser(w, r) == nil {
		return
	}
	o, p := s.uiProject(r)
	if o == nil || !roleAtLeast(s.orgRole(r, o.Slug), "admin") {
		s.uiError(w, r, http.StatusForbidden, "Only admins can manage keys.", nil)
		return
	}
	_, _ = s.pool.Exec(r.Context(), `
		UPDATE ingest_keys SET revoked_at = now()
		WHERE id = $1 AND project_id = $2 AND revoked_at IS NULL`,
		r.PathValue("keyID"), p.ID)
	http.Redirect(w, r, "/"+o.Slug+"/"+p.Slug+"/settings/", http.StatusFound)
}

// --- user settings ---------------------------------------------------------

func (s *Server) uiUserSettings(w http.ResponseWriter, r *http.Request) {
	u := s.uiUser(w, r)
	if u == nil {
		return
	}
	type token struct {
		ID, Name, Prefix, Scope, Created string
		Active                           bool
	}
	tokens := []token{}
	trows, err := s.pool.Query(r.Context(), `
		SELECT id::text, name, token_prefix, scope, created_at::text, (revoked_at IS NULL)
		FROM api_tokens WHERE user_id = $1 ORDER BY created_at DESC`, u.ID)
	if err == nil {
		defer trows.Close()
		for trows.Next() {
			var t token
			_ = trows.Scan(&t.ID, &t.Name, &t.Prefix, &t.Scope, &t.Created, &t.Active)
			tokens = append(tokens, t)
		}
		trows.Close()
	}
	// A freshly minted token is shown once via ?new_token= (prefix match only;
	// the full value travels in the redirect query and is never stored).
	newToken := r.URL.Query().Get("new_token")
	web.Render(w, 200, "app_user_settings", web.PageData{
		Title: "User settings",
		Data: map[string]any{
			"nav":    s.uiNav(r, u, nil, nil, "user"),
			"user":   map[string]string{"name": u.Name, "email": u.Email},
			"tokens": tokens, "new_token": newToken,
		},
	})
}

func (s *Server) uiUpdateProfile(w http.ResponseWriter, r *http.Request) {
	u := s.uiUser(w, r)
	if u == nil {
		return
	}
	if err := r.ParseForm(); err == nil {
		if name := strings.TrimSpace(r.PostFormValue("name")); name != "" {
			_, _ = s.pool.Exec(r.Context(), `UPDATE users SET name = $1 WHERE id = $2`, name, u.ID)
		}
	}
	http.Redirect(w, r, "/user/settings/", http.StatusFound)
}

func (s *Server) uiCreateToken(w http.ResponseWriter, r *http.Request) {
	u := s.uiUser(w, r)
	if u == nil {
		return
	}
	full, prefix, err := auth.NewAPIToken()
	if err != nil {
		s.uiError(w, r, 500, err.Error(), nil)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		name = "Token " + timeNowShort()
	}
	scope := r.PostFormValue("scope")
	if scope != "read" {
		scope = "write"
	}
	_, err = s.pool.Exec(r.Context(), `
		INSERT INTO api_tokens (id, user_id, name, token_hash, token_prefix, scope)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		uuid.NewString(), u.ID, name, auth.HashToken(full), prefix, scope)
	if err != nil {
		s.uiError(w, r, 500, err.Error(), nil)
		return
	}
	http.Redirect(w, r, "/user/settings/?new_token="+full, http.StatusFound)
}

func (s *Server) uiRevokeToken(w http.ResponseWriter, r *http.Request) {
	u := s.uiUser(w, r)
	if u == nil {
		return
	}
	_, _ = s.pool.Exec(r.Context(), `
		UPDATE api_tokens SET revoked_at = now()
		WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`,
		r.PathValue("tokenID"), u.ID)
	http.Redirect(w, r, "/user/settings/", http.StatusFound)
}

// registerUserSettings mounts the /user/settings/ pages on the main mux.
// They live apart from registerWebUI because the /user/settings/ subtree
// overlaps the /{org}/settings/... patterns ("user" could be an {org}), and
// Go's ServeMux rejects such overlaps.
func (s *Server) registerUserSettings(mux *http.ServeMux) {
	page := func(h http.HandlerFunc) http.Handler { return http.HandlerFunc(h) }
	mutate := func(h http.Handler) http.Handler { return s.requireCSRF(h) }

	mux.Handle("GET /user/settings/", page(s.uiUserSettings))
	mux.Handle("POST /user/settings/", mutate(page(s.uiUpdateProfile)))
	// Exact (slashless) "new" endpoint: a trailing-slash subtree here would
	// overlap the {tokenID}/revoke sibling and panic at registration.
	mux.Handle("POST /user/settings/tokens/new", mutate(page(s.uiCreateToken)))
	mux.Handle("POST /user/settings/tokens/{tokenID}/revoke/", mutate(page(s.uiRevokeToken)))
}
