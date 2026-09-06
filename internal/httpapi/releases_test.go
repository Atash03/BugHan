package httpapi

import (
	"bytes"
	"mime/multipart"
	"net/http/httptest"
	"strings"
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

// uploadFile posts a single-file multipart upload the way sentry-cli does.
func (s *Server) uploadFile(t *testing.T, path string, name, dist string, headers []string, content []byte, bearer map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "upload")
	_, _ = fw.Write(content)
	_ = mw.WriteField("name", name)
	if dist != "" {
		_ = mw.WriteField("dist", dist)
	}
	for _, h := range headers {
		_ = mw.WriteField("header", h)
	}
	_ = mw.Close()
	req := httptest.NewRequest("POST", path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	for k, v := range bearer {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestReleaseFileUploadListDelete(t *testing.T) {
	s := newTestServer(t)
	org, projectID, bearer := releasesSetup(t, s)
	s.apiCall(t, "POST", "/api/0/organizations/"+org+"/releases/",
		`{"version":"r1","projects":["`+projectID+`"]}`, jar{}, bearer)
	filesPath := "/api/0/projects/" + org + "/" + projectID + "/releases/r1/files/"

	js := []byte("//# debugId=8e15901e-3eb2-4ba6-8b38-33aa5e97e3a3\n//# sourceMappingURL=app.js.map\nconsole.log(1)")
	rec := s.uploadFile(t, filesPath, "/assets/app.js", "", nil, js, bearer)
	if rec.Code != 201 {
		t.Fatalf("js upload = %d: %s", rec.Code, rec.Body.String())
	}
	var file struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &file); err != nil || file.ID == "" || file.Name != "/assets/app.js" {
		t.Fatalf("upload body = %s (%v)", rec.Body.String(), err)
	}
	if int64(len(js)) != file.Size || file.SHA256 == "" {
		t.Fatalf("size/sha = %d/%q", file.Size, file.SHA256)
	}
	jsID := file.ID

	rec = s.uploadFile(t, filesPath, "/assets/app.js.map", "", nil, []byte(`{"version":3,"sources":[],"mappings":""}`), bearer)
	if rec.Code != 201 {
		t.Fatalf("map upload = %d: %s", rec.Code, rec.Body.String())
	}

	// List shows both artifacts with their metadata.
	rec = s.apiCall(t, "GET", filesPath, "", jar{}, bearer)
	if rec.Code != 200 {
		t.Fatalf("file list = %d", rec.Code)
	}
	var list []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list) != 2 {
		t.Fatalf("file list = %s (%v)", rec.Body.String(), err)
	}
	if list[0].Name != "/assets/app.js" || list[0].Size != int64(len(js)) {
		t.Fatalf("first file = %+v", list[0])
	}

	// Re-upload of identical content is deduped: same id, no second row.
	rec = s.uploadFile(t, filesPath, "/assets/app.js", "", nil, js, bearer)
	if rec.Code != 200 {
		t.Fatalf("identical re-upload = %d: %s", rec.Code, rec.Body.String())
	}
	var again struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &again)
	if again.ID != jsID {
		t.Fatalf("dedupe: got id %s, want %s", again.ID, jsID)
	}

	// Same name, different content → Sentry's 409 contract.
	rec = s.uploadFile(t, filesPath, "/assets/app.js", "", nil, []byte("changed"), bearer)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already exists") {
		t.Fatalf("conflict = %d: %s", rec.Code, rec.Body.String())
	}

	// A dist isolates artifacts: same name under a dist is a new artifact.
	rec = s.uploadFile(t, filesPath, "/assets/app.js", "ios", nil, []byte("changed"), bearer)
	if rec.Code != 201 {
		t.Fatalf("dist upload = %d: %s", rec.Code, rec.Body.String())
	}

	// Delete removes exactly one artifact.
	rec = s.apiCall(t, "DELETE", filesPath+jsID+"/", "", jar{}, bearer)
	if rec.Code != 204 {
		t.Fatalf("delete = %d", rec.Code)
	}
	rec = s.apiCall(t, "GET", filesPath, "", jar{}, bearer)
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 2 {
		t.Fatalf("after delete want 2 files, got %d", len(list))
	}

	// Unknown release → 404.
	rec = s.apiCall(t, "GET", "/api/0/projects/"+org+"/"+projectID+"/releases/none/files/", "", jar{}, bearer)
	if rec.Code != 404 {
		t.Fatalf("unknown release = %d", rec.Code)
	}
}

func TestReleaseFileCaps(t *testing.T) {
	s := newTestServer(t)
	org, projectID, bearer := releasesSetup(t, s)
	s.apiCall(t, "POST", "/api/0/organizations/"+org+"/releases/",
		`{"version":"big","projects":["`+projectID+`"]}`, jar{}, bearer)
	filesPath := "/api/0/projects/" + org + "/" + projectID + "/releases/big/files/"

	// Per-file cap: one byte over the 50 MB limit → 413, nothing stored.
	over := bytes.Repeat([]byte("a"), int(maxFileBytes)+1)
	rec := s.uploadFile(t, filesPath, "/over.js", "", nil, over, bearer)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize file = %d (want 413)", rec.Code)
	}
	rec = s.apiCall(t, "GET", filesPath, "", jar{}, bearer)
	var list []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 0 {
		t.Fatalf("oversize upload stored %d files", len(list))
	}

	// Per-release cap: the sum of file sizes in the release is budgeted.
	old := maxReleaseBytes
	maxReleaseBytes = 200
	defer func() { maxReleaseBytes = old }()
	if rec := s.uploadFile(t, filesPath, "/a.js", "", nil, bytes.Repeat([]byte("a"), 100), bearer); rec.Code != 201 {
		t.Fatalf("first small upload = %d", rec.Code)
	}
	rec = s.uploadFile(t, filesPath, "/b.js", "", nil, bytes.Repeat([]byte("b"), 101), bearer)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-budget release = %d (want 413)", rec.Code)
	}
	// ...but a 100-byte file still fits (total exactly 200).
	rec = s.uploadFile(t, filesPath, "/b.js", "", nil, bytes.Repeat([]byte("b"), 100), bearer)
	if rec.Code != 201 {
		t.Fatalf("at-budget upload = %d", rec.Code)
	}
}
