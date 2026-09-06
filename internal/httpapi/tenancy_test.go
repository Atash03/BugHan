package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/Atash03/BugHan/internal/config"
	"github.com/Atash03/BugHan/internal/testdb"
)

func newTestServer(t *testing.T) *Server {
	pool := testdb.New(t)
	cfg := &config.Config{PublicURL: "http://localhost:8000", SecretKey: "test-secret"}
	return New(cfg, pool, slog.Default())
}

// jar is a minimal cookie jar for tests.
type jar map[string]string

func (j jar) addTo(req *http.Request) {
	for k, v := range j {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
}

func captureCookies(rec *httptest.ResponseRecorder) jar {
	j := jar{}
	for _, c := range rec.Result().Cookies() {
		if c.Value != "" {
			j[c.Name] = c.Value
		}
	}
	return j
}

func (s *Server) postForm(t *testing.T, path string, form url.Values, cookies jar) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	cookies.addTo(req)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func (s *Server) get(t *testing.T, path string, cookies jar, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	cookies.addTo(req)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func (s *Server) apiCall(t *testing.T, method, path, body string, cookies jar, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	cookies.addTo(req)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

var csrfRe = regexp.MustCompile(`name="csrf_token" value="([0-9a-f-]+)"`)

func extractCSRF(body string) string {
	if m := csrfRe.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return ""
}

// login runs the login form and returns the session jar + CSRF token.
func login(t *testing.T, s *Server, email, password string) (jar, string) {
	rec := s.postForm(t, "/auth/login/", url.Values{"email": {email}, "password": {password}}, jar{})
	if rec.Code != 302 {
		t.Fatalf("login = %d: %s", rec.Code, rec.Body.String())
	}
	j := captureCookies(rec)
	landing := s.get(t, "/auth/landing/", j, nil)
	return j, extractCSRF(landing.Body.String())
}

// setupAccount runs the first-run setup form and returns the session jar.
func setupAccount(t *testing.T, s *Server, email string) jar {
	rec := s.postForm(t, "/auth/setup/", url.Values{
		"name": {"Neo"}, "email": {email}, "password": {"hunter2hunter2"}, "org": {"Acme Corp"},
	}, jar{})
	if rec.Code != 302 {
		t.Fatalf("setup status = %d body=%s", rec.Code, rec.Body.String())
	}
	return captureCookies(rec)
}

func TestSetupLoginLogout(t *testing.T) {
	s := newTestServer(t)

	// Fresh install redirects login → setup.
	rec := s.get(t, "/auth/login/", jar{}, nil)
	if rec.Code != 302 || !strings.Contains(rec.Header().Get("Location"), "/auth/setup/") {
		t.Fatalf("expected redirect to setup, got %d %v", rec.Code, rec.Header().Get("Location"))
	}

	sess := setupAccount(t, s, "neo@acme.dev")

	// Landing lists the org.
	rec = s.get(t, "/auth/landing/", sess, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Acme Corp") {
		t.Fatalf("landing = %d; body: %s", rec.Code, rec.Body.String())
	}

	// Second setup attempt redirects away.
	rec = s.postForm(t, "/auth/setup/", url.Values{"name": {"x"}, "email": {"x@x.io"}, "password": {"12345678"}, "org": {"y"}}, jar{})
	if rec.Code != 302 {
		t.Fatalf("second setup should redirect, got %d", rec.Code)
	}

	// Logout kills the session.
	rec = s.postForm(t, "/auth/logout/", url.Values{"csrf_token": {extractCSRF(s.get(t, "/auth/landing/", sess, nil).Body.String())}}, sess)
	if rec.Code != 302 {
		t.Fatalf("logout = %d", rec.Code)
	}
	rec = s.get(t, "/auth/landing/", sess, nil)
	if rec.Code != 302 {
		t.Fatalf("after logout, landing should redirect, got %d", rec.Code)
	}

	// Login again works.
	_, _ = login(t, s, "neo@acme.dev", "hunter2hunter2")

	// Bad password rejected.
	rec = s.postForm(t, "/auth/login/", url.Values{"email": {"neo@acme.dev"}, "password": {"wrong"}}, jar{})
	if rec.Code != 401 {
		t.Fatalf("bad login = %d", rec.Code)
	}
}

func TestPasswordResetFlow(t *testing.T) {
	s := newTestServer(t)
	setupAccount(t, s, "neo@acme.dev")

	// Forgot → notice contains the raw link when SMTP is off.
	rec := s.postForm(t, "/auth/forgot/", url.Values{"email": {"neo@acme.dev"}}, jar{})
	m := regexp.MustCompile(`/auth/reset/([0-9a-f]{64})`).FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatalf("no reset link in response: %s", rec.Body.String())
	}

	// Reset with the token.
	rec = s.postForm(t, "/auth/reset/", url.Values{"token": {m[1]}, "password": {"newpassword1"}}, jar{})
	if rec.Code != 302 {
		t.Fatalf("reset = %d: %s", rec.Code, rec.Body.String())
	}

	// Old password fails; new one works.
	rec = s.postForm(t, "/auth/login/", url.Values{"email": {"neo@acme.dev"}, "password": {"hunter2hunter2"}}, jar{})
	if rec.Code != 401 {
		t.Fatalf("old password should fail, got %d", rec.Code)
	}
	if _, _ = login(t, s, "neo@acme.dev", "newpassword1"); false {
		t.Fatal("unreachable")
	}
}

func TestAPITokenAndOrgFlow(t *testing.T) {
	s := newTestServer(t)
	setupAccount(t, s, "neo@acme.dev")
	sess, csrf := login(t, s, "neo@acme.dev", "hunter2hunter2")

	// CSRF-protected: token mint without CSRF fails.
	rec := s.apiCall(t, "POST", "/api/0/tokens/", `{"name":"ci","scope":"write"}`, sess, nil)
	if rec.Code != 403 {
		t.Fatalf("token mint without CSRF = %d (want 403)", rec.Code)
	}

	rec = s.apiCall(t, "POST", "/api/0/tokens/", `{"name":"ci","scope":"write"}`, sess,
		map[string]string{"X-CSRF-Token": csrf})
	if rec.Code != 201 {
		t.Fatalf("token mint = %d: %s", rec.Code, rec.Body.String())
	}
	var tok struct{ Token string }
	_ = json.Unmarshal(rec.Body.Bytes(), &tok)
	if !strings.HasPrefix(tok.Token, "bghan_") {
		t.Fatalf("token = %q", tok.Token)
	}

	// Bearer auth works for the REST API (no session, no CSRF).
	bearer := map[string]string{"Authorization": "Bearer " + tok.Token}
	rec = s.get(t, "/api/0/", nil, bearer)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "neo@acme.dev") {
		t.Fatalf("whoami = %d: %s", rec.Code, rec.Body.String())
	}

	// Create an org + project via Bearer; DSN built correctly.
	rec = s.apiCall(t, "POST", "/api/0/organizations/", `{"name":"Beta LLC"}`, jar{}, bearer)
	if rec.Code != 201 {
		t.Fatalf("create org = %d: %s", rec.Code, rec.Body.String())
	}
	var org struct{ Slug string }
	_ = json.Unmarshal(rec.Body.Bytes(), &org)

	rec = s.apiCall(t, "POST", "/api/0/organizations/"+org.Slug+"/projects/", `{"name":"Front End"}`, jar{}, bearer)
	if rec.Code != 201 {
		t.Fatalf("create project = %d: %s", rec.Code, rec.Body.String())
	}
	var proj struct {
		ID   string `json:"id"`
		Keys []struct {
			DSN string `json:"dsn"`
		} `json:"keys"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &proj)
	if len(proj.Keys) != 1 {
		t.Fatalf("expected 1 key, got %+v", proj.Keys)
	}
	dsn := proj.Keys[0].DSN
	if !strings.HasPrefix(dsn, "https://") ||
		!strings.Contains(dsn, "@localhost:8000/") ||
		!strings.HasSuffix(dsn, "/"+proj.ID) {
		t.Fatalf("DSN malformed: %q (project %s)", dsn, proj.ID)
	}

	// Invite returns the raw link when SMTP is off.
	rec = s.apiCall(t, "POST", "/api/0/organizations/"+org.Slug+"/invites/",
		`{"email":"kim@acme.dev","role":"member"}`, jar{}, bearer)
	if rec.Code != 201 || !strings.Contains(rec.Body.String(), "invite_link") {
		t.Fatalf("invite = %d: %s", rec.Code, rec.Body.String())
	}

	// Unauthenticated API is 401; bad token 401.
	if rec = s.get(t, "/api/0/", nil, nil); rec.Code != 401 {
		t.Fatalf("whoami unauthenticated = %d", rec.Code)
	}
	if rec = s.get(t, "/api/0/", nil, map[string]string{"Authorization": "Bearer bghan_ffff"}); rec.Code != 401 {
		t.Fatalf("bad token = %d", rec.Code)
	}
}

func TestInviteAcceptFlow(t *testing.T) {
	s := newTestServer(t)
	setupAccount(t, s, "neo@acme.dev")
	sess, csrf := login(t, s, "neo@acme.dev", "hunter2hunter2")

	rec := s.apiCall(t, "POST", "/api/0/tokens/", `{"name":"ci"}`, sess, map[string]string{"X-CSRF-Token": csrf})
	var tok struct{ Token string }
	_ = json.Unmarshal(rec.Body.Bytes(), &tok)
	bearer := map[string]string{"Authorization": "Bearer " + tok.Token}

	rec = s.apiCall(t, "POST", "/api/0/organizations/", `{"name":"Invite Org"}`, jar{}, bearer)
	var org struct{ Slug string }
	_ = json.Unmarshal(rec.Body.Bytes(), &org)

	rec = s.apiCall(t, "POST", "/api/0/organizations/"+org.Slug+"/invites/",
		`{"email":"kim@acme.dev","role":"admin"}`, jar{}, bearer)
	var inv struct {
		InviteLink string `json:"invite_link"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &inv)
	token := inv.InviteLink[strings.LastIndex(inv.InviteLink, "/")+1:]

	// kim signs up (separate browser), then accepts the invite.
	rec = s.postForm(t, "/auth/signup/", url.Values{
		"name": {"Kim"}, "email": {"kim@acme.dev"}, "password": {"kimpassword"},
	}, jar{})
	kim := captureCookies(rec)

	// CSRF mismatch → 403.
	rec = s.postForm(t, "/accept/"+token, url.Values{}, kim)
	if rec.Code != 403 {
		t.Fatalf("accept without CSRF = %d", rec.Code)
	}

	kimLanding := s.get(t, "/auth/landing/", kim, nil)
	kimCSRF := extractCSRF(kimLanding.Body.String())
	rec = s.postForm(t, "/accept/"+token, url.Values{"csrf_token": {kimCSRF}}, kim)
	if rec.Code != 302 {
		t.Fatalf("accept = %d: %s", rec.Code, rec.Body.String())
	}

	// kim now sees the org and holds admin rights on it.
	rec = s.get(t, "/auth/landing/", kim, nil)
	if !strings.Contains(rec.Body.String(), "Invite Org") {
		t.Fatalf("kim landing missing org: %s", rec.Body.String())
	}
	rec = s.apiCall(t, "GET", "/api/0/organizations/"+org.Slug+"/invites/", "", kim, map[string]string{"X-CSRF-Token": kimCSRF})
	if rec.Code != 200 {
		t.Fatalf("kim admin API = %d: %s", rec.Code, rec.Body.String())
	}

	// Second accept fails (single-use).
	rec = s.postForm(t, "/accept/"+token, url.Values{"csrf_token": {kimCSRF}}, kim)
	if rec.Code != 404 {
		t.Fatalf("re-accept = %d (want 404)", rec.Code)
	}
}
