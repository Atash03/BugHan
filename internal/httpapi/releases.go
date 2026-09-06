package httpapi

import (
	"net/http"
	"strings"
)

// registerReleases mounts the classic release-files API (DESIGN.md §10) —
// the surface sentry-cli drives: org-scoped release create (fanning out to
// projects), project-scoped create/list, and release file upload/list/delete.
func (s *Server) registerReleases(mux *http.ServeMux) {
	mutate := func(h http.Handler) http.Handler { return s.requireCSRF(s.requireAuth(h)) }

	// requireOrg / requireProject load the path entity and check the
	// requester's org role before the handler runs.
	requireOrg := func(h http.HandlerFunc, minRole string) http.Handler {
		return s.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			o := s.orgFromPath(r)
			if o == nil {
				writeErr(w, http.StatusNotFound, "organization not found")
				return
			}
			if !roleAtLeast(s.orgRole(r, o.Slug), minRole) {
				writeErr(w, http.StatusForbidden, "insufficient role")
				return
			}
			h(w, r)
		}))
	}
	requireProject := func(h http.HandlerFunc, minRole string) http.Handler {
		return s.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		}))
	}

	mux.Handle("POST /api/0/organizations/{org}/releases/", mutate(http.HandlerFunc(s.handleCreateReleaseOrg)))
	mux.Handle("GET /api/0/organizations/{org}/releases/", requireOrg(s.handleListReleasesOrg, "member"))
	mux.Handle("POST /api/0/projects/{org}/{project}/releases/", mutate(requireProject(s.handleCreateReleaseProject, "admin")))
	mux.Handle("GET /api/0/projects/{org}/{project}/releases/", requireProject(s.handleListReleasesProject, "member"))
}

// handleCreateReleaseOrg implements sentry-cli's opening request:
// POST {"version", "projects": [...]} — one release row per named project.
func (s *Server) handleCreateReleaseOrg(w http.ResponseWriter, r *http.Request) {
	o := s.orgFromPath(r)
	if o == nil {
		writeErr(w, http.StatusNotFound, "organization not found")
		return
	}
	if !roleAtLeast(s.orgRole(r, o.Slug), "admin") {
		writeErr(w, http.StatusForbidden, "insufficient role")
		return
	}
	var in struct {
		Version  string   `json:"version"`
		Projects []string `json:"projects"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	created, conflicted := s.createReleases(r, o.ID, in.Projects, in.Version)
	if len(created) == 0 {
		if conflicted {
			writeJSONStatus(w, http.StatusConflict, map[string]any{
				"detail": "a release with this version already exists",
				"version": in.Version, "projects": created,
			})
			return
		}
		writeErr(w, http.StatusBadRequest, "release version and at least one project are required")
		return
	}
	writeJSONStatus(w, http.StatusCreated, map[string]any{
		"version": in.Version, "projects": created,
		"status": "open", "firstEvent": nil, "lastEvent": nil,
	})
}

// createReleases inserts one releases row per project reference (slug or id).
// Returns the slugs of newly created rows and whether every row already existed.
func (s *Server) createReleases(r *http.Request, orgID string, projectRefs []string, version string) (created []string, allExisted bool) {
	version = strings.TrimSpace(version)
	if version == "" || len(projectRefs) == 0 {
		return nil, false
	}
	created = []string{}
	newRows := 0
	for _, ref := range projectRefs {
		var projID, slug string
		err := s.pool.QueryRow(r.Context(),
			`SELECT id::text, slug FROM projects WHERE org_id = $1 AND (slug = $2 OR id::text = $2)`,
			orgID, strings.TrimSpace(ref)).Scan(&projID, &slug)
		if err != nil {
			continue // unknown project reference: skipped, like Sentry
		}
		ct, err := s.pool.Exec(r.Context(), `
			INSERT INTO releases (id, project_id, version)
			VALUES (gen_random_uuid(), $1, $2)
			ON CONFLICT (project_id, version) DO NOTHING`,
			projID, version)
		if err != nil || ct.RowsAffected() == 0 {
			continue
		}
		newRows++
		created = append(created, slug)
	}
	return created, newRows == 0 && len(projectRefs) > 0
}

func (s *Server) handleListReleasesOrg(w http.ResponseWriter, r *http.Request) {
	o := s.orgFromPath(r)
	rows, err := s.pool.Query(r.Context(), `
		SELECT DISTINCT ON (version) version, first_event_at::text, last_event_at::text, created_at::text
		FROM releases WHERE project_id IN (SELECT id FROM projects WHERE org_id = $1)
		ORDER BY version, created_at DESC`, o.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, scanReleases(rows))
}

// handleCreateReleaseProject is the project-scoped create variant ({"version"}).
func (s *Server) handleCreateReleaseProject(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromPath(r)
	var in struct {
		Version string `json:"version"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	created, allExisted := s.createReleases(r, p.OrgID, []string{p.ID}, in.Version)
	if len(created) == 0 {
		if allExisted {
			writeJSONStatus(w, http.StatusConflict, map[string]any{
				"detail": "a release with this version already exists", "version": in.Version,
			})
			return
		}
		writeErr(w, http.StatusBadRequest, "release version is required")
		return
	}
	writeJSONStatus(w, http.StatusCreated, map[string]any{
		"version": in.Version, "projects": created,
		"status": "open", "firstEvent": nil, "lastEvent": nil,
	})
}

func (s *Server) handleListReleasesProject(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromPath(r)
	rows, err := s.pool.Query(r.Context(), `
		SELECT version, first_event_at::text, last_event_at::text, created_at::text
		FROM releases WHERE project_id = $1
		ORDER BY created_at DESC`, p.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, scanReleases(rows))
}

func scanReleases(rows interface {
	Next() bool
	Scan(dest ...any) error
}) []map[string]any {
	out := []map[string]any{}
	for rows.Next() {
		var version, created string
		var first, last *string
		if err := rows.Scan(&version, &first, &last, &created); err != nil {
			continue
		}
		out = append(out, map[string]any{
			"version":     version,
			"shortVersion": version,
			"status":      "open",
			"firstEvent":  first,
			"lastEvent":   last,
			"dateCreated": created,
		})
	}
	return out
}
