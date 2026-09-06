package httpapi

import (
	"encoding/json"
	"testing"
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
