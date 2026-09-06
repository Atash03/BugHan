package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Atash03/BugHan/internal/auth"
	"github.com/google/uuid"
)

func (s *Server) registerTenancy(mux *http.ServeMux) {
	api := func(h http.HandlerFunc) http.Handler {
		return s.requireAuth(h)
	}
	// State-changing API routes additionally enforce CSRF for session
	// callers; Bearer-token callers are exempt (no ambient credentials).
	mutate := func(h http.Handler) http.Handler {
		return s.requireCSRF(h)
	}
	// requireOrg loads {org} from the path and checks the requester's role.
	requireOrg := func(h http.HandlerFunc, minRole string) http.Handler {
		return api(func(w http.ResponseWriter, r *http.Request) {
			org := s.orgFromPath(r)
			if org == nil {
				writeErr(w, http.StatusNotFound, "organization not found")
				return
			}
			if !roleAtLeast(s.orgRole(r, org.Slug), minRole) {
				writeErr(w, http.StatusForbidden, "insufficient role")
				return
			}
			h(w, r)
		})
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

	mux.Handle("GET /api/0/", api(s.handleWhoami))
	mux.Handle("GET /api/0/organizations/", api(s.handleListOrgs))
	mux.Handle("POST /api/0/organizations/", mutate(s.requireAuth(http.HandlerFunc(s.handleCreateOrg))))
	mux.Handle("GET /api/0/organizations/{org}/", requireOrg(s.handleOrgDetail, "member"))
	mux.Handle("GET /api/0/organizations/{org}/members/", requireOrg(s.handleListMembers, "member"))
	mux.Handle("DELETE /api/0/organizations/{org}/members/{memberID}/", mutate(requireOrg(s.handleRemoveMember, "admin")))
	mux.Handle("POST /api/0/organizations/{org}/invites/", mutate(requireOrg(s.handleCreateInvite, "admin")))
	mux.Handle("GET /api/0/organizations/{org}/invites/", requireOrg(s.handleListInvites, "admin"))
	mux.Handle("DELETE /api/0/organizations/{org}/invites/{inviteID}/", mutate(requireOrg(s.handleDeleteInvite, "admin")))
	mux.Handle("POST /api/0/organizations/{org}/projects/", mutate(requireOrg(s.handleCreateProject, "admin")))
	mux.Handle("GET /api/0/organizations/{org}/projects/", requireOrg(s.handleListProjects, "member"))
	mux.Handle("GET /api/0/projects/{org}/{project}/", requireProject(s.handleProjectDetail, "member"))
	mux.Handle("GET /api/0/projects/{org}/{project}/keys/", requireProject(s.handleListKeys, "member"))
	mux.Handle("POST /api/0/projects/{org}/{project}/keys/", mutate(requireProject(s.handleCreateKey, "admin")))
	mux.Handle("DELETE /api/0/projects/{org}/{project}/keys/{keyID}/", mutate(requireProject(s.handleRevokeKey, "admin")))
	mux.Handle("GET /api/0/tokens/", api(s.handleListTokens))
	mux.Handle("POST /api/0/tokens/", mutate(s.requireAuth(http.HandlerFunc(s.handleCreateToken))))
	mux.Handle("DELETE /api/0/tokens/{tokenID}/", mutate(s.requireAuth(http.HandlerFunc(s.handleRevokeToken))))
}

type org struct {
	ID, Name, Slug string
}

type project struct {
	ID, OrgID, OrgSlug, OrgName, Name, Slug, Platform string
}

func (s *Server) orgFromPath(r *http.Request) *org {
	var o org
	err := s.pool.QueryRow(r.Context(),
		`SELECT id, name, slug FROM organizations WHERE slug = $1 OR id::text = $1`,
		r.PathValue("org")).Scan(&o.ID, &o.Name, &o.Slug)
	if err != nil {
		return nil
	}
	return &o
}

func (s *Server) projectFromPath(r *http.Request) *project {
	var p project
	err := s.pool.QueryRow(r.Context(), `
		SELECT p.id, p.org_id, o.slug, o.name, p.name, p.slug, p.platform
		FROM projects p JOIN organizations o ON o.id = p.org_id
		WHERE o.slug = $1 AND (p.slug = $2 OR p.id::text = $2)`,
		r.PathValue("org"), r.PathValue("project")).Scan(
		&p.ID, &p.OrgID, &p.OrgSlug, &p.OrgName, &p.Name, &p.Slug, &p.Platform)
	if err != nil {
		return nil
	}
	return &p
}

func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	u := auth.FromContext(r.Context())
	writeJSON(w, map[string]any{
		"user":    map[string]string{"id": u.ID, "email": u.Email, "name": u.Name},
		"version": s.version,
	})
}

func (s *Server) handleListOrgs(w http.ResponseWriter, r *http.Request) {
	u := auth.FromContext(r.Context())
	rows, err := s.pool.Query(r.Context(), `
		SELECT o.id, o.name, o.slug, m.role FROM organizations o
		JOIN memberships m ON m.org_id = o.id AND m.user_id = $1
		ORDER BY o.name`, u.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	orgs := []map[string]string{}
	for rows.Next() {
		var id, name, slug, role string
		_ = rows.Scan(&id, &name, &slug, &role)
		orgs = append(orgs, map[string]string{"id": id, "name": name, "slug": slug, "role": role})
	}
	writeJSON(w, orgs)
}

func (s *Server) handleCreateOrg(w http.ResponseWriter, r *http.Request) {
	var in struct{ Name string }
	if !decodeBody(w, r, &in) || strings.TrimSpace(in.Name) == "" {
		return
	}
	if s.cfg.SingleOrg {
		var n int
		_ = s.pool.QueryRow(r.Context(), `SELECT count(*) FROM organizations`).Scan(&n)
		if n > 0 {
			writeErr(w, http.StatusForbidden, "this install is locked to a single organization")
			return
		}
	}
	u := auth.FromContext(r.Context())
	id := uuid.NewString()
	slug := auth.Slugify(in.Name)
	var finalSlug string
	err := s.pool.QueryRow(r.Context(), `
		INSERT INTO organizations (id, name, slug)
		VALUES ($1, $2, $3)
		ON CONFLICT (slug) DO UPDATE SET name = EXCLUDED.name
		RETURNING slug`, id, strings.TrimSpace(in.Name), slug).Scan(&finalSlug)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if _, err := s.pool.Exec(r.Context(),
		`INSERT INTO memberships (id, org_id, user_id, role) VALUES ($1, $2, $3, 'owner')`,
		uuid.NewString(), id, u.ID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSONStatus(w, http.StatusCreated, map[string]string{"id": id, "name": in.Name, "slug": finalSlug})
}

func (s *Server) handleOrgDetail(w http.ResponseWriter, r *http.Request) {
	o := s.orgFromPath(r)
	writeJSON(w, map[string]string{"id": o.ID, "name": o.Name, "slug": o.Slug})
}

func (s *Server) handleListMembers(w http.ResponseWriter, r *http.Request) {
	o := s.orgFromPath(r)
	rows, err := s.pool.Query(r.Context(), `
		SELECT m.id, u.id, u.email, u.name, m.role, m.created_at::text
		FROM memberships m JOIN users u ON u.id = m.user_id
		WHERE m.org_id = $1 ORDER BY m.created_at`, o.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	members := []map[string]string{}
	for rows.Next() {
		var id, uid, email, name, role, created string
		_ = rows.Scan(&id, &uid, &email, &name, &role, &created)
		members = append(members, map[string]string{
			"id": id, "user_id": uid, "email": email, "name": name, "role": role, "joined": created,
		})
	}
	writeJSON(w, members)
}

func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	o := s.orgFromPath(r)
	memberID := r.PathValue("memberID")
	// Never remove the last owner.
	var count int
	if err := s.pool.QueryRow(r.Context(), `
		SELECT count(*) FROM memberships WHERE org_id = $1 AND role = 'owner'`, o.ID).Scan(&count); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var role string
	var uid string
	if err := s.pool.QueryRow(r.Context(), `
		SELECT role, user_id FROM memberships WHERE id = $1 AND org_id = $2`, memberID, o.ID).Scan(&role, &uid); err != nil {
		writeErr(w, 404, "member not found")
		return
	}
	if role == "owner" && count <= 1 {
		writeErr(w, http.StatusBadRequest, "cannot remove the last owner")
		return
	}
	if _, err := s.pool.Exec(r.Context(), `DELETE FROM memberships WHERE id = $1`, memberID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// Auth sessions of a removed member die with the membership.
	_, _ = s.pool.Exec(r.Context(), `DELETE FROM auth_sessions WHERE user_id = $1`, uid)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	o := s.orgFromPath(r)
	var in struct{ Email, Role string }
	if !decodeBody(w, r, &in) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	role := in.Role
	if role == "" {
		role = "member"
	}
	if role != "admin" && role != "member" {
		writeErr(w, http.StatusBadRequest, "role must be admin or member")
		return
	}
	if email == "" {
		writeErr(w, http.StatusBadRequest, "email is required")
		return
	}
	u := auth.FromContext(r.Context())
	raw, _ := auth.NewToken()
	id := uuid.NewString()
	if _, err := s.pool.Exec(r.Context(), `
		INSERT INTO invitations (id, org_id, email, role, token_hash, invited_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, now() + interval '7 days')`,
		id, o.ID, email, role, auth.HashToken(raw), u.ID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	link := s.cfg.PublicURL + "/accept/" + raw
	if err := s.mailer.Send(email, "You're invited to "+o.Name+" on BugHan",
		"Accept your invitation:\n"+link+"\n\nThe link expires in 7 days."); err != nil {
		s.log.Error("send invite email", "err", err)
	}
	resp := map[string]string{"id": id, "email": email, "role": role}
	if !s.mailer.Enabled() {
		resp["invite_link"] = link
	}
	writeJSONStatus(w, http.StatusCreated, resp)
}

func (s *Server) handleListInvites(w http.ResponseWriter, r *http.Request) {
	o := s.orgFromPath(r)
	rows, err := s.pool.Query(r.Context(), `
		SELECT id, email, role, expires_at::text, accepted_at::text
		FROM invitations WHERE org_id = $1 AND expires_at > now() - interval '30 days'
		ORDER BY created_at DESC`, o.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, email, role, expires string
		var accepted *string
		_ = rows.Scan(&id, &email, &role, &expires, &accepted)
		out = append(out, map[string]any{
			"id": id, "email": email, "role": role, "expires": expires, "accepted": accepted,
		})
	}
	writeJSON(w, out)
}

func (s *Server) handleDeleteInvite(w http.ResponseWriter, r *http.Request) {
	o := s.orgFromPath(r)
	if _, err := s.pool.Exec(r.Context(),
		`DELETE FROM invitations WHERE id = $1 AND org_id = $2 AND accepted_at IS NULL`,
		r.PathValue("inviteID"), o.ID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	o := s.orgFromPath(r)
	rows, err := s.pool.Query(r.Context(), `
		SELECT id, name, slug, platform FROM projects WHERE org_id = $1 ORDER BY name`, o.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]string{}
	for rows.Next() {
		var id, name, slug, platform string
		_ = rows.Scan(&id, &name, &slug, &platform)
		out = append(out, map[string]string{"id": id, "name": name, "slug": slug, "platform": platform})
	}
	writeJSON(w, out)
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	o := s.orgFromPath(r)
	var in struct{ Name, Platform string }
	if !decodeBody(w, r, &in) || strings.TrimSpace(in.Name) == "" {
		return
	}
	if in.Platform == "" {
		in.Platform = "javascript"
	}
	id := uuid.NewString()
	slug := auth.Slugify(in.Name)
	var finalSlug, keyID string
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(r.Context())
	if err := tx.QueryRow(r.Context(), `
		INSERT INTO projects (id, org_id, name, slug, platform)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (org_id, slug) DO UPDATE SET name = EXCLUDED.name
		RETURNING slug, id::text`, id, o.ID, strings.TrimSpace(in.Name), slug, in.Platform).
		Scan(&finalSlug, &id); err != nil {
		writeErr(w, 400, "could not create project: "+err.Error())
		return
	}
	keyID = uuid.NewString()
	if _, err := tx.Exec(r.Context(),
		`INSERT INTO ingest_keys (id, project_id, name) VALUES ($1, $2, 'Default')`, keyID, id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	p := &project{ID: id, OrgID: o.ID, OrgSlug: o.Slug, Name: in.Name, Slug: finalSlug, Platform: in.Platform}
	writeJSONStatus(w, http.StatusCreated, map[string]any{
		"id": p.ID, "name": p.Name, "slug": p.Slug, "platform": p.Platform,
		"keys": []map[string]string{{"id": keyID, "name": "Default", "dsn": s.dsnFor(p, keyID)}},
	})
}

func (s *Server) handleProjectDetail(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromPath(r)
	rows, err := s.pool.Query(r.Context(), `
		SELECT id::text, name, (revoked_at IS NULL) FROM ingest_keys
		WHERE project_id = $1 ORDER BY created_at`, p.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	keys := []map[string]any{}
	for rows.Next() {
		var id, name string
		var active bool
		_ = rows.Scan(&id, &name, &active)
		entry := map[string]any{"id": id, "name": name, "active": active}
		if active {
			entry["dsn"] = s.dsnFor(p, id)
		}
		keys = append(keys, entry)
	}
	writeJSON(w, map[string]any{
		"id": p.ID, "name": p.Name, "slug": p.Slug, "platform": p.Platform,
		"organization": p.OrgSlug, "keys": keys,
	})
}

// dsnFor builds the Sentry-format DSN: scheme://key@host/projectID
func (s *Server) dsnFor(p *project, keyID string) string {
	u := s.cfg.PublicURL
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	return "https://" + keyID + "@" + u + "/" + p.ID
}

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromPath(r)
	rows, err := s.pool.Query(r.Context(), `
		SELECT id::text, name, created_at::text, (revoked_at IS NULL)
		FROM ingest_keys WHERE project_id = $1 ORDER BY created_at`, p.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, name, created string
		var active bool
		_ = rows.Scan(&id, &name, &created, &active)
		entry := map[string]any{"id": id, "name": name, "created": created, "active": active}
		if active {
			entry["dsn"] = s.dsnFor(p, id)
		}
		out = append(out, entry)
	}
	writeJSON(w, out)
}

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromPath(r)
	var in struct{ Name string }
	_ = json.NewDecoder(r.Body).Decode(&in)
	if strings.TrimSpace(in.Name) == "" {
		in.Name = "Key " + timeNowShort()
	}
	id := uuid.NewString()
	if _, err := s.pool.Exec(r.Context(),
		`INSERT INTO ingest_keys (id, project_id, name) VALUES ($1, $2, $3)`,
		id, p.ID, strings.TrimSpace(in.Name)); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSONStatus(w, http.StatusCreated, map[string]any{
		"id": id, "name": in.Name, "dsn": s.dsnFor(p, id), "active": true,
	})
}

func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromPath(r)
	if _, err := s.pool.Exec(r.Context(), `
		UPDATE ingest_keys SET revoked_at = now()
		WHERE id = $1 AND project_id = $2 AND revoked_at IS NULL`,
		r.PathValue("keyID"), p.ID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	u := auth.FromContext(r.Context())
	rows, err := s.pool.Query(r.Context(), `
		SELECT id::text, name, token_prefix, scope, created_at::text, (revoked_at IS NULL)
		FROM api_tokens WHERE user_id = $1 ORDER BY created_at DESC`, u.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, name, prefix, scope, created string
		var active bool
		_ = rows.Scan(&id, &name, &prefix, &scope, &created, &active)
		out = append(out, map[string]any{
			"id": id, "name": name, "prefix": prefix, "scope": scope, "created": created, "active": active,
		})
	}
	writeJSON(w, out)
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	var in struct{ Name, Scope string }
	if !decodeBody(w, r, &in) || strings.TrimSpace(in.Name) == "" {
		return
	}
	if in.Scope == "" {
		in.Scope = "write"
	}
	if in.Scope != "read" && in.Scope != "write" {
		writeErr(w, http.StatusBadRequest, "scope must be read or write")
		return
	}
	u := auth.FromContext(r.Context())
	full, prefix, err := auth.NewAPIToken()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	id := uuid.NewString()
	if _, err := s.pool.Exec(r.Context(), `
		INSERT INTO api_tokens (id, user_id, name, token_hash, token_prefix, scope)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		id, u.ID, strings.TrimSpace(in.Name), auth.HashToken(full), prefix, in.Scope); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSONStatus(w, http.StatusCreated, map[string]any{
		"id": id, "name": in.Name, "scope": in.Scope, "prefix": prefix,
		"token": full, // shown exactly once
	})
}

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	u := auth.FromContext(r.Context())
	if _, err := s.pool.Exec(r.Context(), `
		UPDATE api_tokens SET revoked_at = now()
		WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`,
		r.PathValue("tokenID"), u.ID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
