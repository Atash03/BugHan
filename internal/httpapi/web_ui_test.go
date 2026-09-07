package httpapi

import (
	"net/url"
	"strings"
	"testing"
)

// T8 web UI slice tests: shell auth gating, dashboard, issues list + detail,
// triage/comment POSTs, projects + onboarding, and settings pages.

func uiLogin(t *testing.T, s *Server, fx *apiFixture) (jar, string) {
	t.Helper()
	return login(t, s, fx.UserEmail, "hunter2hunter2")
}

func mustContain(t *testing.T, body, want string) {
	t.Helper()
	if !strings.Contains(body, want) {
		t.Fatalf("page missing %q", want)
	}
	if strings.Contains(body, "template error") {
		t.Fatalf("page contains a template execution error")
	}
}

func TestWebUIRequiresLogin(t *testing.T) {
	s := newTestServer(t)
	rec := s.get(t, "/some-org/", jar{}, nil)
	if rec.Code != http302() {
		t.Fatalf("anon dashboard = %d (want 302 to login)", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/auth/login/" {
		t.Fatalf("anon dashboard redirects to %q (want /auth/login/)", loc)
	}
	rec = s.get(t, "/user/settings/", jar{}, nil)
	if rec.Code != http302() {
		t.Fatalf("anon user settings = %d (want 302)", rec.Code)
	}
}

func http302() int { return 302 }

func TestWebUIDashboard(t *testing.T) {
	s := newTestServer(t)
	fx := apiSetup(t, s)
	sess, _ := uiLogin(t, s, fx)

	rec := s.get(t, "/"+fx.OrgSlug+"/", sess, nil)
	if rec.Code != 200 {
		t.Fatalf("dashboard = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	mustContain(t, body, "Triage Org")
	mustContain(t, body, "Unresolved issues")
	mustContain(t, body, "BugHan")

	// Unknown org 404s without leaking existence.
	rec = s.get(t, "/no-such-org/", sess, nil)
	if rec.Code != 404 {
		t.Fatalf("unknown org = %d (want 404)", rec.Code)
	}
}

func TestWebUIIssuesAndDetail(t *testing.T) {
	s := newTestServer(t)
	fx := apiSetup(t, s)
	sess, csrf := uiLogin(t, s, fx)

	fx.postErrorEnvelope(t, s, "9ec79c33ec9942ab8353589fcb2e04dc", "TypeError",
		"Cannot read properties of undefined", "user-1", "2026-09-06T10:00:00Z")
	issueID := fx.firstIssueID(t, s)

	// Issues list shows the ingested issue with its sparkline + filters.
	rec := s.get(t, "/"+fx.OrgSlug+"/"+fx.ProjectSlug+"/issues/", sess, nil)
	if rec.Code != 200 {
		t.Fatalf("issues list = %d: %s", rec.Code, rec.Body.String())
	}
	mustContain(t, rec.Body.String(), "Cannot read properties of undefined")
	// Sparkline bars render (guards template-scope regressions in pct args).
	mustContain(t, rec.Body.String(), `<i style="height:`)

	// Filter round-trips.
	rec = s.get(t, "/"+fx.OrgSlug+"/"+fx.ProjectSlug+"/issues/?status=resolved", sess, nil)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "Cannot read properties of undefined") {
		t.Fatalf("resolved filter leaks unresolved issue: %d", rec.Code)
	}

	// Issue detail renders stacktrace context + triage controls.
	rec = s.get(t, "/"+fx.OrgSlug+"/"+fx.ProjectSlug+"/issues/"+issueID+"/", sess, nil)
	if rec.Code != 200 {
		t.Fatalf("issue detail = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	mustContain(t, body, "TypeError")
	mustContain(t, body, "Resolve")
	mustContain(t, body, "Activity")

	// Triage via form POST resolves the issue (CSRF-protected).
	rec = s.postForm(t, "/"+fx.OrgSlug+"/"+fx.ProjectSlug+"/issues/"+issueID+"/triage/",
		url.Values{"csrf_token": {csrf}, "status": {"resolved"}}, sess)
	if rec.Code != 302 {
		t.Fatalf("triage POST = %d: %s", rec.Code, rec.Body.String())
	}
	rec = s.get(t, "/"+fx.OrgSlug+"/"+fx.ProjectSlug+"/issues/?status=resolved", sess, nil)
	mustContain(t, rec.Body.String(), "Cannot read properties of undefined")

	// Comment lands on the activity feed.
	rec = s.postForm(t, "/"+fx.OrgSlug+"/"+fx.ProjectSlug+"/issues/"+issueID+"/comment/",
		url.Values{"csrf_token": {csrf}, "body": {"repro found on checkout"}}, sess)
	if rec.Code != 302 {
		t.Fatalf("comment POST = %d: %s", rec.Code, rec.Body.String())
	}
	rec = s.get(t, "/"+fx.OrgSlug+"/"+fx.ProjectSlug+"/issues/"+issueID+"/", sess, nil)
	mustContain(t, rec.Body.String(), "repro found on checkout")

	// Bulk reopen from the list.
	rec = s.postForm(t, "/"+fx.OrgSlug+"/"+fx.ProjectSlug+"/issues/bulk/",
		url.Values{"csrf_token": {csrf}, "ids": {issueID}, "action": {"unresolved"}}, sess)
	if rec.Code != 302 {
		t.Fatalf("bulk POST = %d: %s", rec.Code, rec.Body.String())
	}
	rec = s.get(t, "/"+fx.OrgSlug+"/"+fx.ProjectSlug+"/issues/", sess, nil)
	mustContain(t, rec.Body.String(), "Cannot read properties of undefined")

	// CSRF-less mutation is rejected.
	rec = s.postForm(t, "/"+fx.OrgSlug+"/"+fx.ProjectSlug+"/issues/"+issueID+"/triage/",
		url.Values{"status": {"ignored"}}, sess)
	if rec.Code != 403 {
		t.Fatalf("CSRF-less triage = %d (want 403)", rec.Code)
	}
}

func TestWebUIProjectsOnboardingSettings(t *testing.T) {
	s := newTestServer(t)
	fx := apiSetup(t, s)
	sess, csrf := uiLogin(t, s, fx)

	// Projects list + creation → onboarding wizard with DSN.
	rec := s.get(t, "/"+fx.OrgSlug+"/projects/", sess, nil)
	if rec.Code != 200 {
		t.Fatalf("projects = %d: %s", rec.Code, rec.Body.String())
	}
	mustContain(t, rec.Body.String(), "Web App")
	rec = s.postForm(t, "/"+fx.OrgSlug+"/projects/new/",
		url.Values{"csrf_token": {csrf}, "name": {"Mobile Web"}, "platform": {"javascript"}}, sess)
	if rec.Code != 302 {
		t.Fatalf("project create = %d: %s", rec.Code, rec.Body.String())
	}
	onboard := rec.Header().Get("Location")
	if !strings.HasSuffix(onboard, "/onboarding/") {
		t.Fatalf("project create redirects to %q (want onboarding)", onboard)
	}
	rec = s.get(t, onboard, sess, nil)
	if rec.Code != 200 {
		t.Fatalf("onboarding = %d: %s", rec.Code, rec.Body.String())
	}
	mustContain(t, rec.Body.String(), "Sentry.init")

	// Project settings: keys + DSN visible, new key round-trips.
	rec = s.get(t, "/"+fx.OrgSlug+"/"+fx.ProjectSlug+"/settings/", sess, nil)
	if rec.Code != 200 {
		t.Fatalf("project settings = %d: %s", rec.Code, rec.Body.String())
	}
	mustContain(t, rec.Body.String(), "Ingest keys")
	rec = s.postForm(t, "/"+fx.OrgSlug+"/"+fx.ProjectSlug+"/settings/keys/new",
		url.Values{"csrf_token": {csrf}, "name": {"rotated"}}, sess)
	if rec.Code != 302 {
		t.Fatalf("key create = %d: %s", rec.Code, rec.Body.String())
	}
	rec = s.get(t, "/"+fx.OrgSlug+"/"+fx.ProjectSlug+"/settings/", sess, nil)
	mustContain(t, rec.Body.String(), "rotated")

	// Org settings: members listed, invite round-trips.
	rec = s.get(t, "/"+fx.OrgSlug+"/settings/", sess, nil)
	if rec.Code != 200 {
		t.Fatalf("org settings = %d: %s", rec.Code, rec.Body.String())
	}
	mustContain(t, rec.Body.String(), "neo@acme.dev")
	rec = s.postForm(t, "/"+fx.OrgSlug+"/settings/invite/",
		url.Values{"csrf_token": {csrf}, "email": {"sam@acme.dev"}, "role": {"member"}}, sess)
	if rec.Code != 302 {
		t.Fatalf("invite = %d: %s", rec.Code, rec.Body.String())
	}
	rec = s.get(t, "/"+fx.OrgSlug+"/settings/", sess, nil)
	mustContain(t, rec.Body.String(), "sam@acme.dev")

	// User settings: profile + token lifecycle (token shown once).
	rec = s.get(t, "/user/settings/", sess, nil)
	if rec.Code != 200 {
		t.Fatalf("user settings = %d: %s", rec.Code, rec.Body.String())
	}
	mustContain(t, rec.Body.String(), "API tokens")
	rec = s.postForm(t, "/user/settings/tokens/new",
		url.Values{"csrf_token": {csrf}, "name": {"ci"}, "scope": {"read"}}, sess)
	if rec.Code != 302 {
		t.Fatalf("token create = %d: %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "new_token=bghan_") {
		t.Fatalf("token create redirects to %q (want new_token)", loc)
	}
	rec = s.get(t, loc, sess, nil)
	mustContain(t, rec.Body.String(), "bghan_")
}
