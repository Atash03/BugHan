package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Atash03/BugHan/internal/auth"
	"github.com/google/uuid"
)

// releasesSetup provisions an account, org, project, and API token, returning
// the org slug, project id, and Bearer headers (the sentry-cli auth shape).
func releasesSetup(t *testing.T, s *Server) (orgSlug, projectID string, bearer map[string]string) {
	t.Helper()
	setupAccount(t, s, "neo@acme.dev")
	sess, csrf := login(t, s, "neo@acme.dev", "hunter2hunter2")
	rec := s.apiCall(t, "POST", "/api/0/tokens/", `{"name":"ci"}`, sess, map[string]string{"X-CSRF-Token": csrf})
	var tok struct {
		Token string
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &tok)
	bearer = map[string]string{"Authorization": "Bearer " + tok.Token}

	rec = s.apiCall(t, "POST", "/api/0/organizations/", `{"name":"Acme"}`, jar{}, bearer)
	var org struct {
		Slug string
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &org)
	if org.Slug == "" {
		t.Fatalf("org create failed: %d %s", rec.Code, rec.Body.String())
	}

	rec = s.apiCall(t, "POST", "/api/0/organizations/"+org.Slug+"/projects/", `{"name":"Web App"}`, jar{}, bearer)
	var proj struct {
		ID string
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == "" {
		t.Fatalf("project create failed: %d %s", rec.Code, rec.Body.String())
	}
	return org.Slug, proj.ID, bearer
}

// TestCreateReleaseOrgScoped is the tracer bullet: sentry-cli opens every
// sourcemaps upload with an org-scoped release create naming its projects.
func TestCreateReleaseOrgScoped(t *testing.T) {
	s := newTestServer(t)
	org, projectID, bearer := releasesSetup(t, s)

	rec := s.apiCall(t, "POST", "/api/0/organizations/"+org+"/releases/",
		`{"version":"abc123","projects":["`+projectID+`"]}`, jar{}, bearer)
	if rec.Code != 201 {
		t.Fatalf("release create = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Version != "abc123" {
		t.Fatalf("body = %s (%v)", rec.Body.String(), err)
	}

	// The release must be visible on the project (per-project release rows).
	rec = s.apiCall(t, "GET", "/api/0/projects/"+org+"/"+projectID+"/releases/", "", jar{}, bearer)
	if rec.Code != 200 {
		t.Fatalf("project releases list = %d: %s", rec.Code, rec.Body.String())
	}
	var list []struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list) != 1 || list[0].Version != "abc123" {
		t.Fatalf("project releases = %s (%v)", rec.Body.String(), err)
	}
}

func TestCreateReleaseConflictAndAuth(t *testing.T) {
	s := newTestServer(t)
	org, projectID, bearer := releasesSetup(t, s)

	path := "/api/0/organizations/" + org + "/releases/"
	body := `{"version":"abc123","projects":["` + projectID + `"]}`

	// First create succeeds; re-running the same sentry-cli step is a conflict.
	if rec := s.apiCall(t, "POST", path, body, jar{}, bearer); rec.Code != 201 {
		t.Fatalf("first create = %d", rec.Code)
	}
	rec := s.apiCall(t, "POST", path, body, jar{}, bearer)
	if rec.Code != http.StatusConflict {
		t.Fatalf("re-create = %d: %s", rec.Code, rec.Body.String())
	}

	// No Bearer token → 401.
	if rec := s.apiCall(t, "POST", path, body, jar{}, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no auth = %d", rec.Code)
	}

	// Unrelated user's token → 403 (valid token, no org membership). The
	// outsider is inserted directly: first-run setup can't add a second user.
	outside := outsiderBearer(t, s)
	if rec := s.apiCall(t, "POST", path, body, jar{}, outside); rec.Code != http.StatusForbidden {
		t.Fatalf("outsider = %d", rec.Code)
	}

	// Empty version → 400.
	if rec := s.apiCall(t, "POST", path, `{"projects":["`+projectID+`"]}`, jar{}, bearer); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty version = %d", rec.Code)
	}
}

// outsiderBearer inserts a user who belongs to no organization and mints them
// a valid API token directly; the tests use it for the 403 path.
func outsiderBearer(t *testing.T, s *Server) map[string]string {
	t.Helper()
	ctx := context.Background()
	hash, err := auth.HashPassword("hunter2hunter2")
	if err != nil {
		t.Fatal(err)
	}
	uid := uuid.NewString()
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO users (id, email, name, password_hash) VALUES ($1, 'mallory@evil.io', 'Mallory', $2)`,
		uid, hash); err != nil {
		t.Fatal(err)
	}
	full, prefix, err := auth.NewAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO api_tokens (id, user_id, name, token_hash, token_prefix, scope)
		VALUES ($1, $2, 'm', $3, $4, 'write')`,
		uuid.NewString(), uid, auth.HashToken(full), prefix); err != nil {
		t.Fatal(err)
	}
	return map[string]string{"Authorization": "Bearer " + full}
}
