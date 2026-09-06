package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Atash03/BugHan/internal/sourcemap"
	"github.com/google/uuid"
)

// sha256hex is the storage form of artifact checksums.
func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

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

	// Release files (the artifact upload surface sentry-cli drives).
	mux.Handle("POST /api/0/projects/{org}/{project}/releases/{version}/files/",
		mutate(requireProject(s.handleUploadReleaseFile, "admin")))
	mux.Handle("GET /api/0/projects/{org}/{project}/releases/{version}/files/",
		requireProject(s.handleListReleaseFiles, "member"))
	mux.Handle("DELETE /api/0/projects/{org}/{project}/releases/{version}/files/{fileID}/",
		mutate(requireProject(s.handleDeleteReleaseFile, "admin")))
}

// Upload caps (DESIGN.md §10): 50 MB per file, 500 MB per release. Vars so
// tests can shrink them.
var (
	maxFileBytes    int64 = 50 << 20
	maxReleaseBytes int64 = 500 << 20
)

const errFileExists = "A file matching this name already exists for the given release"

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

// handleUploadReleaseFile implements sentry-cli's single-file multipart
// upload: fields `file` + `name` + optional `dist` and repeatable `header`
// ("Name: Value") pairs. Identical content re-uploads dedupe; a same-name
// artifact with different content is Sentry's 409 conflict.
func (s *Server) handleUploadReleaseFile(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromPath(r)
	version := r.PathValue("version")
	var exists bool
	if err := s.pool.QueryRow(r.Context(),
		`SELECT true FROM releases WHERE project_id = $1 AND version = $2`,
		p.ID, version).Scan(&exists); err != nil {
		writeErr(w, http.StatusNotFound, "release not found")
		return
	}

	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid multipart body: "+err.Error())
		return
	}
	fhs := r.MultipartForm.File["file"]
	if len(fhs) != 1 {
		writeErr(w, http.StatusBadRequest, "Missing uploaded file")
		return
	}
	fh := fhs[0]

	name := r.FormValue("name")
	if name == "" {
		name = fh.Filename
	}
	if name == "" || name == "file" {
		writeErr(w, http.StatusBadRequest, "File name must be specified")
		return
	}
	if strings.ContainsAny(name[strings.LastIndex(name, "/")+1:], "\n\t\r\f\v\\") {
		writeErr(w, http.StatusBadRequest, "File name must not contain special whitespace characters")
		return
	}

	headers := map[string]string{}
	for _, hv := range r.MultipartForm.Value["header"] {
		k, v, ok := strings.Cut(hv, ":")
		if !ok {
			writeErr(w, http.StatusBadRequest, "header value was not formatted correctly")
			return
		}
		headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if fh.Header.Get("Content-Type") != "" {
		headers["Content-Type"] = fh.Header.Get("Content-Type")
	}
	dist := r.FormValue("dist")

	// Read once, capped: refuse files over the cap before they reach
	// Postgres (the +1 read detects an oversize body without buffering it).
	part, err := fh.Open()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "unreadable file part")
		return
	}
	defer part.Close()
	body, err := io.ReadAll(io.LimitReader(part, maxFileBytes+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read error: "+err.Error())
		return
	}
	if int64(len(body)) > maxFileBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "file exceeds the 50 MB per-file limit")
		return
	}
	sum := sha256hex(body)
	content := int64(len(body))

	var used int64
	if err := s.pool.QueryRow(r.Context(),
		`SELECT COALESCE(sum(size), 0) FROM release_files WHERE project_id = $1 AND release = $2`,
		p.ID, version).Scan(&used); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if used+content > maxReleaseBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "release exceeds the 500 MB storage limit")
		return
	}

	var existingID, existingSHA string
	err = s.pool.QueryRow(r.Context(), `
		SELECT id::text, sha256 FROM release_files
		WHERE project_id = $1 AND release = $2 AND dist = $3 AND name = $4`,
		p.ID, version, dist, name).Scan(&existingID, &existingSHA)
	switch {
	case err == nil && existingSHA == sum:
		// Same artifact uploaded twice (CI retry): dedupe, return existing.
		s.writeReleaseFileByID(w, r, existingID)
		return
	case err == nil:
		writeJSONStatus(w, http.StatusConflict, map[string]string{"detail": errFileExists})
		return
	}

	debugID, smURL := sourcemap.ExtractArtifact(name, body)
	hdrsJSON, _ := json.Marshal(headers)
	id := uuid.NewString()
	if _, err := s.pool.Exec(r.Context(), `
		INSERT INTO release_files (id, project_id, release, dist, name, headers, body, size, sha256, debug_id, sourcemap_url)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		id, p.ID, version, dist, name, hdrsJSON, body, content, sum, debugID, smURL); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSONStatus(w, http.StatusCreated, serializeReleaseFile(id, name, dist, headers, content, sum))
}

func (s *Server) writeReleaseFileByID(w http.ResponseWriter, r *http.Request, id string) {
	var name, dist, sum string
	var size int64
	var hdrs map[string]string
	err := s.pool.QueryRow(r.Context(), `
		SELECT name, dist, sha256, size, headers FROM release_files WHERE id = $1 AND project_id = $2`,
		id, s.projectFromPath(r).ID).Scan(&name, &dist, &sum, &size, &hdrs)
	if err != nil {
		writeErr(w, 404, "file not found")
		return
	}
	writeJSONStatus(w, http.StatusOK, serializeReleaseFile(id, name, dist, hdrs, size, sum))
}

func serializeReleaseFile(id, name, dist string, headers map[string]string, size int64, sha string) map[string]any {
	var distOut any
	if dist != "" {
		distOut = dist
	}
	return map[string]any{
		"id": id, "name": name, "dist": distOut, "size": size,
		"sha256": sha, "headers": headers, "dateCreated": time.Now().UTC().Format(time.RFC3339),
	}
}

func (s *Server) handleListReleaseFiles(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromPath(r)
	var exists bool
	if err := s.pool.QueryRow(r.Context(),
		`SELECT true FROM releases WHERE project_id = $1 AND version = $2`,
		p.ID, r.PathValue("version")).Scan(&exists); err != nil {
		writeErr(w, http.StatusNotFound, "release not found")
		return
	}
	rows, err := s.pool.Query(r.Context(), `
		SELECT id::text, name, dist, size, sha256, headers, created_at::text
		FROM release_files WHERE project_id = $1 AND release = $2 ORDER BY name`,
		p.ID, r.PathValue("version"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, name, dist, sha, created string
		var size int64
		var headers map[string]string
		if err := rows.Scan(&id, &name, &dist, &size, &sha, &headers, &created); err != nil {
			continue
		}
		entry := serializeReleaseFile(id, name, dist, headers, size, sha)
		entry["dateCreated"] = created
		out = append(out, entry)
	}
	writeJSON(w, out)
}

func (s *Server) handleDeleteReleaseFile(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromPath(r)
	ct, err := s.pool.Exec(r.Context(),
		`DELETE FROM release_files WHERE id = $1 AND project_id = $2 AND release = $3`,
		r.PathValue("fileID"), p.ID, r.PathValue("version"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if ct.RowsAffected() == 0 {
		writeErr(w, http.StatusNotFound, "file not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
