package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/Atash03/BugHan/internal/auth"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// registerIssues wires the issues API: listing, triage, activity, and the
// per-project data wipe (ticket T5).
func (s *Server) registerIssues(mux *http.ServeMux) {
	// Reads need member; triage mutations stay member too (CONTEXT.md roles).
	api := func(h http.HandlerFunc) http.Handler { return s.requireAuth(h) }
	// Mutations additionally enforce CSRF for session callers (Bearer exempt)
	// and reject read-scope API tokens.
	mutate := func(h http.Handler) http.Handler {
		return s.requireCSRF(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if principalScope(r.Context()) == "read" {
				writeErr(w, http.StatusForbidden, "API token has read-only scope")
				return
			}
			h.ServeHTTP(w, r)
		}))
	}
	requireProject := func(h http.HandlerFunc, minRole string) http.Handler {
		return api(func(w http.ResponseWriter, r *http.Request) {
			p := s.projectFromPath(r)
			if p == nil {
				writeErr(w, http.StatusNotFound, "project not found")
				return
			}
			if !roleAtLeast(s.orgRole(r, p.OrgSlug), minRole) {
				writeErr(w, http.StatusForbidden, "insufficient role")
				return
			}
			h(w, r)
		})
	}
	// requireIssue resolves {issue} and checks org membership for it. A
	// non-member gets 404 (no existence leak); an under-privileged member
	// gets 403.
	requireIssue := func(h http.HandlerFunc, minRole string) http.Handler {
		return api(func(w http.ResponseWriter, r *http.Request) {
			ref := s.issueFromPath(r)
			if ref == nil {
				writeErr(w, http.StatusNotFound, "issue not found")
				return
			}
			role := s.orgRole(r, ref.OrgSlug)
			if role == "" {
				writeErr(w, http.StatusNotFound, "issue not found")
				return
			}
			if !roleAtLeast(role, minRole) {
				writeErr(w, http.StatusForbidden, "insufficient role")
				return
			}
			h(w, r.WithContext(context.WithValue(r.Context(), issueCtxKey{}, ref)))
		})
	}

	mux.Handle("GET /api/0/projects/{org}/{project}/issues/", requireProject(s.handleListIssues, "member"))
	mux.Handle("GET /api/0/projects/{org}/{project}/tags/", requireProject(s.handleProjectTags, "member"))
	mux.Handle("GET /api/0/projects/{org}/{project}/views/", requireProject(s.handleListViews, "member"))
	mux.Handle("POST /api/0/projects/{org}/{project}/views/", mutate(requireProject(s.handleCreateView, "member")))
	mux.Handle("DELETE /api/0/projects/{org}/{project}/views/{viewID}/", mutate(requireProject(s.handleDeleteView, "member")))
	mux.Handle("PUT /api/0/projects/{org}/{project}/issues/bulk/", mutate(requireProject(s.handleBulkIssues, "member")))
	mux.Handle("POST /api/0/projects/{org}/{project}/delete-data/", mutate(requireProject(s.handleDeleteProjectData, "admin")))
	mux.Handle("GET /api/0/issues/{issue}/", requireIssue(s.handleIssueDetail, "member"))
	mux.Handle("GET /api/0/issues/{issue}/events/", requireIssue(s.handleListIssueEvents, "member"))
	mux.Handle("GET /api/0/issues/{issue}/events/{event}/", requireIssue(s.handleEventDetail, "member"))
	mux.Handle("POST /api/0/issues/{issue}/comments/", mutate(requireIssue(s.handleAddComment, "member")))
	mux.Handle("GET /api/0/issues/{issue}/activity/", requireIssue(s.handleIssueActivity, "member"))
	mux.Handle("PUT /api/0/issues/{issue}/", mutate(requireIssue(s.handleUpdateIssue, "member")))
}

type issueCtxKey struct{}

// issueRef is the access-checked issue resolved from the path.
type issueRef struct{ ID, ProjectID, OrgID, OrgSlug string }

func (s *Server) issueFromPath(r *http.Request) *issueRef {
	id := r.PathValue("issue")
	if _, err := uuid.Parse(id); err != nil {
		return nil
	}
	var ref issueRef
	err := s.pool.QueryRow(r.Context(), `
		SELECT i.id::text, i.project_id::text, p.org_id::text, o.slug
		FROM issues i
		JOIN projects p ON p.id = i.project_id
		JOIN organizations o ON o.id = p.org_id
		WHERE i.id = $1`, id).Scan(&ref.ID, &ref.ProjectID, &ref.OrgID, &ref.OrgSlug)
	if err != nil {
		return nil
	}
	return &ref
}

// pageParams parses limit/offset (limit clamped 1..100, default 25).
func pageParams(r *http.Request) (limit, offset int) {
	limit, offset = 25, 0
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil {
		limit = v
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && v >= 0 {
		offset = v
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 100 {
		limit = 100
	}
	return limit, offset
}

// issueSortCols whitelists the sort parameter (always descending).
var issueSortCols = map[string]string{
	"last_seen":  "i.last_seen",
	"first_seen": "i.first_seen",
	"count":      "i.count",
	"user_count": "i.user_count",
}

// issueSelectCols is the shared issues-row projection (aliased table `i`,
// optional assignee join on `u`).
const issueSelectCols = `i.id::text, i.fingerprint, i.title, i.culprit, i.type, i.level,
	i.status, i.substatus, i.first_seen::text, i.last_seen::text,
	i.count, i.user_count,
	COALESCE(u.id::text, ''), COALESCE(u.email, ''), COALESCE(u.name, '')`

// buildIssueWhere assembles the WHERE clause for issue queries from the
// request's filter params (aliasing issues as `i`). Returns ok=false after
// writing a 400 when a filter is malformed.
func (s *Server) buildIssueWhere(w http.ResponseWriter, r *http.Request, projectID string) (string, []any, bool) {
	q := r.URL.Query()
	var conds []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}

	conds = append(conds, "i.project_id = "+arg(projectID))
	if statuses := q["status"]; len(statuses) > 0 {
		conds = append(conds, "i.status = ANY("+arg(statuses)+")")
	}
	if levels := q["level"]; len(levels) > 0 {
		conds = append(conds, "i.level = ANY("+arg(levels)+")")
	}
	if envs := q["environment"]; len(envs) > 0 {
		conds = append(conds, "EXISTS (SELECT 1 FROM events_part e WHERE e.issue_id = i.id AND e.environment = ANY("+arg(envs)+"))")
	}
	if releases := q["release"]; len(releases) > 0 {
		conds = append(conds, "EXISTS (SELECT 1 FROM events_part e WHERE e.issue_id = i.id AND e.release = ANY("+arg(releases)+"))")
	}
	for _, tf := range q["tag"] {
		k, v, found := strings.Cut(tf, ":")
		if !found || k == "" {
			writeErr(w, http.StatusBadRequest, "tag filter must be key:value")
			return "", nil, false
		}
		obj, _ := json.Marshal(map[string]string{k: v})
		conds = append(conds, "EXISTS (SELECT 1 FROM events_part e WHERE e.issue_id = i.id AND e.tags @> "+arg(string(obj))+"::jsonb)")
	}
	if query := q.Get("query"); query != "" {
		// Literal substring match: escape LIKE metacharacters in the input.
		pat := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(query) + "%"
		conds = append(conds, "(i.title ILIKE "+arg(pat)+" OR i.culprit ILIKE "+arg(pat)+")")
	}
	return "WHERE " + strings.Join(conds, " AND "), args, true
}

func (s *Server) handleListIssues(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromPath(r)
	limit, offset := pageParams(r)

	sortCol, ok := issueSortCols[r.URL.Query().Get("sort")]
	if !ok {
		if r.URL.Query().Get("sort") != "" {
			writeErr(w, http.StatusBadRequest, "sort must be one of last_seen, first_seen, count, user_count")
			return
		}
		sortCol = issueSortCols["last_seen"]
	}

	where, args, ok := s.buildIssueWhere(w, r, p.ID)
	if !ok {
		return
	}

	var total int
	if err := s.pool.QueryRow(r.Context(),
		`SELECT count(*) FROM issues i `+where, args...).Scan(&total); err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	rows, err := s.pool.Query(r.Context(), `
		SELECT `+issueSelectCols+`
		FROM issues i
		LEFT JOIN users u ON u.id = i.assignee_id
		`+where+`
		ORDER BY `+sortCol+` DESC
		LIMIT $`+strconv.Itoa(len(args)+1)+` OFFSET $`+strconv.Itoa(len(args)+2),
		append(args, limit, offset)...)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()

	data := []map[string]any{}
	for rows.Next() {
		data = append(data, scanIssueRow(rows))
	}
	writeJSON(w, map[string]any{
		"data": data,
		"meta": map[string]int{"total": total, "limit": limit, "offset": offset},
	})
}

type rowScanner interface{ Scan(dest ...any) error }

// scanIssueRow maps one issues row (with optional assignee) to its API shape.
func scanIssueRow(sc rowScanner) map[string]any {
	var id, fingerprint, title, culprit, typ, level, status, substatus string
	var firstSeen, lastSeen string
	var count int64
	var userCount int
	var assigneeID, assigneeEmail, assigneeName string
	if err := sc.Scan(&id, &fingerprint, &title, &culprit, &typ, &level,
		&status, &substatus, &firstSeen, &lastSeen, &count, &userCount,
		&assigneeID, &assigneeEmail, &assigneeName); err != nil {
		return map[string]any{"error": err.Error()}
	}
	issue := map[string]any{
		"id":          id,
		"fingerprint": fingerprint,
		"title":       title,
		"culprit":     culprit,
		"type":        typ,
		"level":       level,
		"status":      status,
		"substatus":   substatus,
		"first_seen":  firstSeen,
		"last_seen":   lastSeen,
		"count":       count,
		"user_count":  userCount,
	}
	if assigneeID != "" {
		issue["assignee"] = map[string]string{"id": assigneeID, "email": assigneeEmail, "name": assigneeName}
	} else {
		issue["assignee"] = nil
	}
	return issue
}

// principalID returns the authenticated user's id (session or Bearer owner).
func principalID(r *http.Request) string {
	if u := auth.FromContext(r.Context()); u != nil {
		return u.ID
	}
	return ""
}

// handleBulkIssues applies one triage action (status and/or assignment) to
// several issues of the project at once; unknown ids are skipped.
func (s *Server) handleBulkIssues(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs        []string        `json:"ids"`
		Status     string          `json:"status"`
		AssignedTo json.RawMessage `json:"assignedTo"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	if len(in.IDs) == 0 {
		writeErr(w, http.StatusBadRequest, "ids is required")
		return
	}
	if in.Status == "" && len(in.AssignedTo) == 0 {
		writeErr(w, http.StatusBadRequest, "nothing to update: give status and/or assignedTo")
		return
	}
	p := s.projectFromPath(r)

	// Resolve the assignment target once: nil means unassign.
	var assigneeID *string
	if len(in.AssignedTo) > 0 {
		if string(in.AssignedTo) == "null" {
			assigneeID = nil
		} else {
			var to string
			if err := json.Unmarshal(in.AssignedTo, &to); err != nil {
				writeErr(w, http.StatusBadRequest, "assignedTo must be \"me\", a member email/id, or null")
				return
			}
			id, err := s.resolveAssignee(r, &issueRef{OrgID: p.OrgID}, to)
			if err != nil {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
			assigneeID = &id
		}
	}

	sets := []string{}
	args := []any{}
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if in.Status != "" {
		switch in.Status {
		case "unresolved", "resolved", "ignored":
		default:
			writeErr(w, http.StatusBadRequest, "status must be unresolved, resolved or ignored")
			return
		}
		sets = append(sets, "status = "+arg(in.Status), "substatus = ''")
	}
	if in.AssignedTo != nil {
		if assigneeID == nil {
			sets = append(sets, "assignee_id = NULL")
		} else {
			sets = append(sets, "assignee_id = "+arg(*assigneeID))
		}
	}

	ids := make([]string, 0, len(in.IDs))
	for _, id := range in.IDs {
		if _, err := uuid.Parse(id); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid issue id "+id)
			return
		}
		ids = append(ids, id)
	}
	args = append(args, ids)
	idsParam := "$" + strconv.Itoa(len(args))
	args = append(args, p.ID)
	projectParam := "$" + strconv.Itoa(len(args))

	ct, err := s.pool.Exec(r.Context(),
		`UPDATE issues SET `+strings.Join(sets, ", ")+`
		 WHERE id = ANY(`+idsParam+`::uuid[]) AND project_id = `+projectParam, args...)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	// Record one activity entry per touched issue (bulk entries carry no
	// previous value; the single-issue PUT does).
	actor := principalID(r)
	target := ` FROM issues WHERE id = ANY($3::uuid[]) AND project_id = $4`
	if in.Status != "" {
		_, _ = s.pool.Exec(r.Context(), `
			INSERT INTO issue_activity (issue_id, author_id, type, data)
			SELECT id, NULLIF($1, '')::uuid, 'status', jsonb_build_object('to', $2::text)`+target,
			actor, in.Status, ids, p.ID)
	}
	if in.AssignedTo != nil {
		if assigneeID != nil {
			_, _ = s.pool.Exec(r.Context(), `
				INSERT INTO issue_activity (issue_id, author_id, type, data)
				SELECT id, NULLIF($1, '')::uuid, 'assignment',
				       jsonb_build_object('assignee', $2::text)`+target,
				actor, *assigneeID, ids, p.ID)
		} else {
			_, _ = s.pool.Exec(r.Context(), `
				INSERT INTO issue_activity (issue_id, author_id, type, data)
				SELECT id, NULLIF($1, '')::uuid, 'assignment', '{"assignee":null}'::jsonb`+target,
				actor, ids, p.ID)
		}
	}

	writeJSON(w, map[string]int{"updated": int(ct.RowsAffected())})
}

// handleIssueDetail returns one issue in the list-row shape.
func (s *Server) handleIssueDetail(w http.ResponseWriter, r *http.Request) {
	ref := r.Context().Value(issueCtxKey{}).(*issueRef)
	s.writeIssue(w, r, ref.ID)
}

// handleListIssueEvents returns an issue's events, newest first.
func (s *Server) handleListIssueEvents(w http.ResponseWriter, r *http.Request) {
	ref := r.Context().Value(issueCtxKey{}).(*issueRef)
	limit, offset := pageParams(r)

	var total int
	if err := s.pool.QueryRow(r.Context(),
		`SELECT count(*) FROM events_part WHERE issue_id = $1`, ref.ID).Scan(&total); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	rows, err := s.pool.Query(r.Context(), `
		SELECT id::text, issue_id::text, timestamp::text, received_at::text,
		       platform, level, environment, COALESCE(release, ''), COALESCE(dist, ''),
		       title, culprit, COALESCE(message, ''), type, COALESCE(user_hash, '')
		FROM events_part
		WHERE issue_id = $1
		ORDER BY timestamp DESC
		LIMIT $2 OFFSET $3`, ref.ID, limit, offset)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()

	data := []map[string]any{}
	for rows.Next() {
		var id, issueID, ts, received, platform, level, environment, release, dist string
		var title, culprit, message, typ, userHash string
		if err := rows.Scan(&id, &issueID, &ts, &received, &platform, &level,
			&environment, &release, &dist, &title, &culprit, &message, &typ, &userHash); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		data = append(data, map[string]any{
			"id": id, "issue_id": issueID, "timestamp": ts, "received_at": received,
			"platform": platform, "level": level, "environment": environment,
			"release": release, "dist": dist, "title": title, "culprit": culprit,
			"message": message, "type": typ, "user_hash": userHash,
		})
	}
	writeJSON(w, map[string]any{
		"data": data,
		"meta": map[string]int{"total": total, "limit": limit, "offset": offset},
	})
}

// handleAddComment posts a note to the issue's activity feed.
func (s *Server) handleAddComment(w http.ResponseWriter, r *http.Request) {
	var in struct{ Body string }
	if !decodeBody(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Body) == "" {
		writeErr(w, http.StatusBadRequest, "comment body is required")
		return
	}
	ref := r.Context().Value(issueCtxKey{}).(*issueRef)
	actor := principalID(r)
	s.recordActivity(r, ref.ID, actor, "note", strings.TrimSpace(in.Body), map[string]any{})

	// Return the created entry (newest note for this actor).
	var id, body, createdAt string
	var data map[string]any
	err := s.pool.QueryRow(r.Context(), `
		SELECT a.id::text, a.body, a.data, a.created_at::text
		FROM issue_activity a
		WHERE a.issue_id = $1 AND a.type = 'note' AND a.author_id = NULLIF($2, '')::uuid
		ORDER BY a.created_at DESC, a.id DESC
		LIMIT 1`, ref.ID, actor).Scan(&id, &body, &data, &createdAt)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSONStatus(w, http.StatusCreated, map[string]any{
		"id": id, "type": "note", "body": body, "data": data,
		"author":   s.memberBrief(r, actor),
		"issue_id": ref.ID, "created_at": createdAt,
	})
}

// memberBrief returns {id,email,name} for a user id, or nil.
func (s *Server) memberBrief(r *http.Request, userID string) any {
	if userID == "" {
		return nil
	}
	var email, name string
	if err := s.pool.QueryRow(r.Context(),
		`SELECT email, name FROM users WHERE id = $1`, userID).Scan(&email, &name); err != nil {
		return nil
	}
	return map[string]string{"id": userID, "email": email, "name": name}
}

// handleDeleteProjectData wipes all telemetry for one project: events,
// transactions, sessions, feedback, rollups, releases, and the issues
// themselves (fresh start; unlike the retention sweep, issues do not
// survive). Ingest keys and saved views are kept. Admin only.
func (s *Server) handleDeleteProjectData(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromPath(r)
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(r.Context())

	stmts := []string{
		// Children of issues first (FKs), then the issues themselves.
		`DELETE FROM event_rollups WHERE issue_id IN (SELECT id FROM issues WHERE project_id = $1)`,
		`DELETE FROM issue_user_hashes WHERE issue_id IN (SELECT id FROM issues WHERE project_id = $1)`,
		`DELETE FROM issue_activity WHERE issue_id IN (SELECT id FROM issues WHERE project_id = $1)`,
		`DELETE FROM events_part WHERE project_id = $1`,
		`DELETE FROM issues WHERE project_id = $1`,
		`DELETE FROM transactions_part WHERE project_id = $1`,
		`DELETE FROM sessions_part WHERE project_id = $1`,
		`DELETE FROM feedbacks WHERE project_id = $1`,
		`DELETE FROM session_rollups WHERE project_id = $1`,
		`DELETE FROM project_event_rollups WHERE project_id = $1`,
		`DELETE FROM releases WHERE project_id = $1`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(r.Context(), stmt, p.ID); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.log.Info("project event data wiped", "project", p.ID, "by", principalID(r))
	w.WriteHeader(http.StatusNoContent)
}

// handleListViews returns the project's saved views.
func (s *Server) handleListViews(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromPath(r)
	rows, err := s.pool.Query(r.Context(), `
		SELECT v.id::text, v.name, v.query, v.created_at::text,
		       COALESCE(u.id::text, ''), COALESCE(u.email, ''), COALESCE(u.name, '')
		FROM saved_views v
		LEFT JOIN users u ON u.id = v.created_by
		WHERE v.project_id = $1
		ORDER BY v.created_at`, p.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	views := []map[string]any{}
	for rows.Next() {
		views = append(views, scanViewRow(rows))
	}
	writeJSON(w, views)
}

func scanViewRow(sc rowScanner) map[string]any {
	var id, name, query, createdAt, byID, byEmail, byName string
	if err := sc.Scan(&id, &name, &query, &createdAt, &byID, &byEmail, &byName); err != nil {
		return map[string]any{"error": err.Error()}
	}
	var createdBy any
	if byID != "" {
		createdBy = map[string]string{"id": byID, "email": byEmail, "name": byName}
	}
	return map[string]any{
		"id": id, "name": name, "query": query, "created_at": createdAt, "created_by": createdBy,
	}
}

// handleCreateView saves a named, bookmarkable issue-list query.
func (s *Server) handleCreateView(w http.ResponseWriter, r *http.Request) {
	var in struct{ Name, Query string }
	if !decodeBody(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	p := s.projectFromPath(r)
	u := auth.FromContext(r.Context())
	var id string
	err := s.pool.QueryRow(r.Context(), `
		INSERT INTO saved_views (project_id, name, query, created_by)
		VALUES ($1, $2, $3, NULLIF($4, '')::uuid)
		RETURNING id::text`, p.ID, strings.TrimSpace(in.Name), in.Query, u.ID).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			writeErr(w, http.StatusBadRequest, "a view with this name already exists")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	var createdAt string
	_ = s.pool.QueryRow(r.Context(), `SELECT created_at::text FROM saved_views WHERE id = $1`, id).Scan(&createdAt)
	writeJSONStatus(w, http.StatusCreated, map[string]any{
		"id": id, "name": strings.TrimSpace(in.Name), "query": in.Query,
		"created_at": createdAt, "created_by": s.memberBrief(r, u.ID),
	})
}

// handleDeleteView removes a saved view.
func (s *Server) handleDeleteView(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromPath(r)
	ct, err := s.pool.Exec(r.Context(),
		`DELETE FROM saved_views WHERE id = $1 AND project_id = $2`,
		r.PathValue("viewID"), p.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if ct.RowsAffected() == 0 {
		writeErr(w, http.StatusNotFound, "view not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleIssueActivity returns the issue's activity feed, newest first.
func (s *Server) handleIssueActivity(w http.ResponseWriter, r *http.Request) {
	ref := r.Context().Value(issueCtxKey{}).(*issueRef)
	limit, offset := pageParams(r)

	rows, err := s.pool.Query(r.Context(), `
		SELECT a.id::text, a.type, a.body, a.data, a.created_at::text,
		       COALESCE(u.id::text, ''), COALESCE(u.email, ''), COALESCE(u.name, '')
		FROM issue_activity a
		LEFT JOIN users u ON u.id = a.author_id
		WHERE a.issue_id = $1
		ORDER BY a.created_at DESC, a.id DESC
		LIMIT $2 OFFSET $3`, ref.ID, limit, offset)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()

	feed := []map[string]any{}
	for rows.Next() {
		var id, typ, body, createdAt string
		var data map[string]any
		var authorID, authorEmail, authorName string
		if err := rows.Scan(&id, &typ, &body, &data, &createdAt, &authorID, &authorEmail, &authorName); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		entry := map[string]any{
			"id": id, "type": typ, "body": body, "data": data, "created_at": createdAt,
		}
		if authorID != "" {
			entry["author"] = map[string]string{"id": authorID, "email": authorEmail, "name": authorName}
		} else {
			entry["author"] = nil
		}
		feed = append(feed, entry)
	}
	writeJSON(w, feed)
}

// handleEventDetail returns one event with its full payload plus breadcrumbs,
// tags, and contexts extracted for convenience.
func (s *Server) handleEventDetail(w http.ResponseWriter, r *http.Request) {
	ref := r.Context().Value(issueCtxKey{}).(*issueRef)
	eventID := r.PathValue("event")
	if _, err := uuid.Parse(eventID); err != nil {
		writeErr(w, http.StatusNotFound, "event not found")
		return
	}
	var ev map[string]any
	var payload, symbolicated []byte
	err := s.pool.QueryRow(r.Context(), `
		SELECT json_build_object(
			'id', id::text, 'issue_id', issue_id::text,
			'timestamp', timestamp::text, 'received_at', received_at::text,
			'platform', platform, 'level', level, 'environment', environment,
			'release', COALESCE(release, ''), 'dist', COALESCE(dist, ''),
			'title', title, 'culprit', culprit, 'message', COALESCE(message, ''),
			'type', type, 'user_hash', COALESCE(user_hash, ''), 'trace_id', COALESCE(trace_id::text, ''),
			'tags', COALESCE(tags, '{}'::jsonb), 'payload', payload,
			'symbolicated', symbolicated),
		       payload, COALESCE(symbolicated, 'null'::jsonb)
		FROM events_part
		WHERE id = $1 AND issue_id = $2
		ORDER BY timestamp DESC
		LIMIT 1`, eventID, ref.ID).Scan(&ev, &payload, &symbolicated)
	if err != nil {
		writeErr(w, http.StatusNotFound, "event not found")
		return
	}

	// Convenience views over the raw payload: breadcrumbs and contexts.
	var parsed struct {
		Breadcrumbs any `json:"breadcrumbs"`
		Contexts    any `json:"contexts"`
	}
	_ = json.Unmarshal(payload, &parsed)
	if parsed.Breadcrumbs == nil {
		ev["breadcrumbs"] = []any{}
	} else {
		ev["breadcrumbs"] = parsed.Breadcrumbs
	}
	if parsed.Contexts == nil {
		ev["contexts"] = map[string]any{}
	} else {
		ev["contexts"] = parsed.Contexts
	}
	var sym any
	_ = json.Unmarshal(symbolicated, &sym)
	ev["symbolicated"] = sym

	writeJSON(w, ev)
}

// handleProjectTags returns tag facets: per key, the top values by count of
// distinct issues affected, within the current issue filters.
func (s *Server) handleProjectTags(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromPath(r)
	where, args, ok := s.buildIssueWhere(w, r, p.ID)
	if !ok {
		return
	}
	rows, err := s.pool.Query(r.Context(), `
		SELECT ev.tags, ev.issue_id::text
		FROM events_part ev
		JOIN issues i ON i.id = ev.issue_id
		`+where, args...)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()

	// key -> value -> set of distinct issue ids
	sets := map[string]map[string]map[string]bool{}
	for rows.Next() {
		var tags map[string]string
		var issueID string
		if err := rows.Scan(&tags, &issueID); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		for k, v := range tags {
			if sets[k] == nil {
				sets[k] = map[string]map[string]bool{}
			}
			if sets[k][v] == nil {
				sets[k][v] = map[string]bool{}
			}
			sets[k][v][issueID] = true
		}
	}

	// Keys ordered by total distinct issues desc, then name; values likewise.
	type kv struct {
		value string
		count int
	}
	out := []map[string]any{}
	keys := make([]string, 0, len(sets))
	totals := map[string]int{}
	for k := range sets {
		keys = append(keys, k)
		for v, issueSet := range sets[k] {
			_ = v
			totals[k] += len(issueSet)
		}
	}
	sort.Slice(keys, func(a, b int) bool {
		if totals[keys[a]] != totals[keys[b]] {
			return totals[keys[a]] > totals[keys[b]]
		}
		return keys[a] < keys[b]
	})
	if len(keys) > 10 {
		keys = keys[:10]
	}
	for _, k := range keys {
		vals := make([]kv, 0, len(sets[k]))
		for v, issueSet := range sets[k] {
			vals = append(vals, kv{v, len(issueSet)})
		}
		sort.Slice(vals, func(a, b int) bool {
			if vals[a].count != vals[b].count {
				return vals[a].count > vals[b].count
			}
			return vals[a].value < vals[b].value
		})
		if len(vals) > 10 {
			vals = vals[:10]
		}
		values := []map[string]any{}
		for _, v := range vals {
			values = append(values, map[string]any{"value": v.value, "count": v.count})
		}
		out = append(out, map[string]any{"key": k, "values": values})
	}
	writeJSON(w, out)
}

// handleUpdateIssue applies a triage action to one issue: status change
// and/or assignment. Fields are independent; an empty object is an error.
// Both actions are recorded on the issue's activity feed.
func (s *Server) handleUpdateIssue(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Status     string          `json:"status"`
		AssignedTo json.RawMessage `json:"assignedTo"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	if in.Status == "" && len(in.AssignedTo) == 0 {
		writeErr(w, http.StatusBadRequest, "nothing to update: give status and/or assignedTo")
		return
	}
	ref := r.Context().Value(issueCtxKey{}).(*issueRef)
	actor := principalID(r)

	if in.Status != "" {
		switch in.Status {
		case "unresolved", "resolved", "ignored":
		default:
			writeErr(w, http.StatusBadRequest, "status must be unresolved, resolved or ignored")
			return
		}
		var from string
		if err := s.pool.QueryRow(r.Context(),
			`SELECT status FROM issues WHERE id = $1`, ref.ID).Scan(&from); err != nil {
			writeErr(w, 404, "issue not found")
			return
		}
		if _, err := s.pool.Exec(r.Context(),
			`UPDATE issues SET status = $1, substatus = '' WHERE id = $2`,
			in.Status, ref.ID); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if from != in.Status {
			s.recordActivity(r, ref.ID, actor, "status", "",
				map[string]string{"from": from, "to": in.Status})
		}
	}

	if len(in.AssignedTo) > 0 {
		if string(in.AssignedTo) == "null" {
			if _, err := s.pool.Exec(r.Context(),
				`UPDATE issues SET assignee_id = NULL WHERE id = $1`, ref.ID); err != nil {
				writeErr(w, 500, err.Error())
				return
			}
			s.recordActivity(r, ref.ID, actor, "assignment", "", map[string]any{"assignee": nil})
		} else {
			var to string
			if err := json.Unmarshal(in.AssignedTo, &to); err != nil {
				writeErr(w, http.StatusBadRequest, "assignedTo must be \"me\", a member email/id, or null")
				return
			}
			assigneeID, err := s.resolveAssignee(r, ref, to)
			if err != nil {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
			if _, err := s.pool.Exec(r.Context(),
				`UPDATE issues SET assignee_id = $1 WHERE id = $2`, assigneeID, ref.ID); err != nil {
				writeErr(w, 500, err.Error())
				return
			}
			s.recordActivity(r, ref.ID, actor, "assignment", "", map[string]any{"assignee": assigneeID})
		}
	}

	s.writeIssue(w, r, ref.ID)
}

// recordActivity appends one entry to an issue's activity feed.
func (s *Server) recordActivity(r *http.Request, issueID, authorID, typ, body string, data any) {
	dataJSON, err := json.Marshal(data)
	if err != nil {
		dataJSON = []byte(`{}`)
	}
	_, _ = s.pool.Exec(r.Context(), `
		INSERT INTO issue_activity (issue_id, author_id, type, body, data)
		VALUES ($1, NULLIF($2, '')::uuid, $3, $4, $5)`,
		issueID, authorID, typ, body, dataJSON)
}

// resolveAssignee maps "me", a member email, or a user id (within the issue's
// org) to a user id.
func (s *Server) resolveAssignee(r *http.Request, ref *issueRef, to string) (string, error) {
	if to == "me" {
		to = principalID(r)
	}
	var id string
	err := s.pool.QueryRow(r.Context(), `
		SELECT u.id::text FROM users u
		JOIN memberships m ON m.user_id = u.id AND m.org_id = $2
		WHERE (lower(u.email) = $1 OR u.id::text = $1)`,
		strings.ToLower(strings.TrimSpace(to)), ref.OrgID).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("assignee %q is not a member of this organization", to)
	}
	return id, nil
}

// writeIssue loads one issue row and writes it in the API shape.
func (s *Server) writeIssue(w http.ResponseWriter, r *http.Request, issueID string) {
	row := s.pool.QueryRow(r.Context(), `
		SELECT `+issueSelectCols+`
		FROM issues i
		LEFT JOIN users u ON u.id = i.assignee_id
		WHERE i.id = $1`, issueID)
	writeJSON(w, scanIssueRow(row))
}
