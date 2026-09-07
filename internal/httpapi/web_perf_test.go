package httpapi

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// T9 web UI slice tests (DESIGN.md §11, §13): performance summary +
// transaction detail, trace waterfall, releases list/detail + artifact
// upload, and the project feedback list.

// seedT9 provisions a project with one trace (two transactions + one linked
// error), a release with session health + one artifact, and one feedback row.
func seedT9(t *testing.T, s *Server, fx *apiFixture) (traceID, issueID string) {
	t.Helper()
	ctx := context.Background()

	fx.postErrorEnvelope(t, s, "9ec79c33ec9942ab8353589fcb2e04dc", "TypeError",
		"Cannot read properties of undefined", "user-1", "2026-09-06T10:00:00Z")
	issueID = fx.firstIssueID(t, s)
	traceID = uuid.NewString()

	if _, err := s.pool.Exec(ctx,
		`UPDATE events_part SET trace_id = $1, release = 'web@1.0.0' WHERE issue_id = $2`,
		traceID, issueID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO releases (id, project_id, version, first_event_at, last_event_at)
		VALUES ($1, $2, 'web@1.0.0', now(), now())`,
		uuid.NewString(), fx.ProjectID); err != nil {
		t.Fatal(err)
	}

	hour := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	for i, d := range []float64{120, 340} {
		start := hour.Add(time.Duration(i) * time.Second)
		var tr any
		if i == 0 {
			tr = traceID
		}
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO transactions_part (id, project_id, timestamp, start_ts, duration_ms,
				trace_id, span_id, name, source, status, environment, release, dist, measurements, payload)
			VALUES ($1,$2,$3,$4,$5,$6,'root','GET /list','view','ok','production','web@1.0.0','',
				'{"lcp":{"value":1800,"unit":"millisecond"},"cls":{"value":0.05,"unit":"none"}}',
				'{"spans":[{"span_id":"sp1","parent_span_id":"","op":"db","description":"SELECT 1","start_timestamp":1,"timestamp":1.05},{"span_id":"sp2","parent_span_id":"sp1","op":"db.parse","description":"parse rows","start_timestamp":1.01,"timestamp":1.02}]}')`,
			uuid.NewString(), fx.ProjectID, start, start, d, tr); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO session_rollups (project_id, release, environment, hour, total, crashed)
		VALUES ($1,'web@1.0.0','production',$2,10,2)`, fx.ProjectID, hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO release_files (id, project_id, release, dist, name, headers, body, size, sha256, debug_id)
		VALUES ($1,$2,'web@1.0.0','','/assets/app.js','{}','console.log(1)',14,'abc','8e15901e-3eb2-4ba6-8b38-33aa5e97e3a3')`,
		uuid.NewString(), fx.ProjectID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO feedbacks (id, project_id, issue_id, name, contact_email, message, url, payload)
		VALUES ($1,$2,$3,'Ada','ada@example.com','checkout button does nothing','https://x/','{}')`,
		uuid.NewString(), fx.ProjectID, issueID); err != nil {
		t.Fatal(err)
	}
	return traceID, issueID
}

func TestWebUIPerformance(t *testing.T) {
	s := newTestServer(t)
	fx := apiSetup(t, s)
	sess, _ := uiLogin(t, s, fx)
	base := "/" + fx.OrgSlug + "/" + fx.ProjectSlug + "/performance/"

	// Empty state (no transactions yet) renders without template errors.
	rec := s.get(t, base, sess, nil)
	if rec.Code != 200 {
		t.Fatalf("performance empty = %d: %s", rec.Code, rec.Body.String())
	}
	mustContain(t, rec.Body.String(), "Transactions")
	mustContain(t, rec.Body.String(), "tracesSampleRate")

	traceID, _ := seedT9(t, s, fx)
	_ = traceID

	// Summary: transaction row with percentiles, failure rate, vitals strip.
	rec = s.get(t, base+"?statsPeriod=24h", sess, nil)
	if rec.Code != 200 {
		t.Fatalf("performance = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	mustContain(t, body, "GET /list")
	mustContain(t, body, "LCP")
	mustContain(t, body, "Volume")

	// Transaction detail: histogram + recent events with trace links.
	rec = s.get(t, base+"?name=GET+%2Flist&statsPeriod=24h", sess, nil)
	if rec.Code != 200 {
		t.Fatalf("transaction detail = %d: %s", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()
	mustContain(t, body, "Duration histogram")
	mustContain(t, body, "Recent events")
	mustContain(t, body, "trace")

	// Anon browsers bounce to login like the rest of the UI family.
	if rec := s.get(t, base, jar{}, nil); rec.Code != 302 {
		t.Fatalf("anon performance = %d (want 302)", rec.Code)
	}
}

func TestWebUITrace(t *testing.T) {
	s := newTestServer(t)
	fx := apiSetup(t, s)
	sess, _ := uiLogin(t, s, fx)
	traceID, issueID := seedT9(t, s, fx)
	_ = issueID
	path := "/" + fx.OrgSlug + "/" + fx.ProjectSlug + "/traces/" + traceID + "/"

	rec := s.get(t, path, sess, nil)
	if rec.Code != 200 {
		t.Fatalf("trace = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	mustContain(t, body, "waterfall")
	mustContain(t, body, "SELECT 1")
	mustContain(t, body, "Self time")
	mustContain(t, body, "Cannot read properties of undefined")
	mustContain(t, body, "LCP")

	// Span drawer: ?span= renders the selected span's detail.
	rec = s.get(t, path+"?span=sp1", sess, nil)
	mustContain(t, rec.Body.String(), "parent")

	// Op filter that matches nothing dims every span (0 / N).
	rec = s.get(t, path+"?op=does-not-exist", sess, nil)
	mustContain(t, rec.Body.String(), "0 / 2 spans")

	// Unknown-but-wellformed trace id 404s; garbage 400s.
	unknown := "/" + fx.OrgSlug + "/" + fx.ProjectSlug + "/traces/" + uuid.NewString() + "/"
	if rec := s.get(t, unknown, sess, nil); rec.Code != 404 {
		t.Fatalf("unknown trace = %d (want 404)", rec.Code)
	}
	bad := "/" + fx.OrgSlug + "/" + fx.ProjectSlug + "/traces/not-a-uuid/"
	if rec := s.get(t, bad, sess, nil); rec.Code != 400 {
		t.Fatalf("bad trace id = %d (want 400)", rec.Code)
	}
}

func TestWebUIReleases(t *testing.T) {
	s := newTestServer(t)
	fx := apiSetup(t, s)
	sess, csrf := uiLogin(t, s, fx)
	_, _ = seedT9(t, s, fx)
	base := "/" + fx.OrgSlug + "/" + fx.ProjectSlug + "/releases/"

	rec := s.get(t, base, sess, nil)
	if rec.Code != 200 {
		t.Fatalf("releases = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	mustContain(t, body, "web@1.0.0")
	mustContain(t, body, "Crash-free")
	mustContain(t, body, "new")

	detail := base + "web@1.0.0/"
	rec = s.get(t, detail, sess, nil)
	if rec.Code != 200 {
		t.Fatalf("release detail = %d: %s", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()
	mustContain(t, body, "Issues first seen")
	mustContain(t, body, "Cannot read properties of undefined")
	mustContain(t, body, "Source-map files")
	mustContain(t, body, "/assets/app.js")
	mustContain(t, body, "Upload artifact")

	// Unknown release 404s.
	if rec := s.get(t, base+"nope-9.9.9/", sess, nil); rec.Code != 404 {
		t.Fatalf("unknown release = %d (want 404)", rec.Code)
	}

	// Admin upload round-trips through the UI form and shows in the files table.
	rec = postUIMultipart(t, s, detail+"files/", sess, csrf, "extra.js", "console.log(2)")
	if rec.Code != 302 {
		t.Fatalf("ui upload = %d: %s", rec.Code, rec.Body.String())
	}
	rec = s.get(t, detail, sess, nil)
	mustContain(t, rec.Body.String(), "extra.js")

	// CSRF-less upload is rejected.
	rec = postUIMultipart(t, s, detail+"files/", sess, "", "evil.js", "x")
	if rec.Code != 403 {
		t.Fatalf("CSRF-less upload = %d (want 403)", rec.Code)
	}

	// Members (non-admin) cannot upload.
	fx.addMember(t, s, "trixie@acme.dev")
	memberSess, memberCSRF := login(t, s, "trixie@acme.dev", "hunter2hunter2")
	if rec := postUIMultipart(t, s, detail+"files/", memberSess, memberCSRF, "m.js", "x"); rec.Code != 403 {
		t.Fatalf("member upload = %d (want 403)", rec.Code)
	}
}

// postUIMultipart posts a single-file multipart form to a UI upload route.
func postUIMultipart(t *testing.T, s *Server, path string, cookies jar, csrf, name, content string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", name)
	_, _ = fw.Write([]byte(content))
	if csrf != "" {
		_ = mw.WriteField("csrf_token", csrf)
	}
	_ = mw.Close()
	req := httptest.NewRequest("POST", path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	cookies.addTo(req)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestWebUIFeedback(t *testing.T) {
	s := newTestServer(t)
	fx := apiSetup(t, s)
	sess, _ := uiLogin(t, s, fx)
	_, issueID := seedT9(t, s, fx)
	path := "/" + fx.OrgSlug + "/" + fx.ProjectSlug + "/feedback/"

	rec := s.get(t, path, sess, nil)
	if rec.Code != 200 {
		t.Fatalf("feedback = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	mustContain(t, body, "checkout button does nothing")
	mustContain(t, body, "ada@example.com")
	mustContain(t, body, "/"+fx.OrgSlug+"/"+fx.ProjectSlug+"/issues/"+issueID+"/")

	// Empty project renders the empty state.
	fx2 := apiSetupOrg(t, s, "Second Org", "zed@acme.dev", "hunter2hunter2")
	_ = fx2
	sess2, _ := login(t, s, "zed@acme.dev", "hunter2hunter2")
	rec = s.get(t, "/second-org/web-app/feedback/", sess2, nil)
	if rec.Code != 200 {
		t.Fatalf("feedback empty = %d: %s", rec.Code, rec.Body.String())
	}
	mustContain(t, rec.Body.String(), "No feedback yet")
}

func TestWebUISidebarLinksT9(t *testing.T) {
	s := newTestServer(t)
	fx := apiSetup(t, s)
	sess, _ := uiLogin(t, s, fx)

	// The project sidebar links the T9 sections (guards chrome regressions).
	rec := s.get(t, "/"+fx.OrgSlug+"/"+fx.ProjectSlug+"/issues/", sess, nil)
	body := rec.Body.String()
	for _, want := range []string{
		"/" + fx.OrgSlug + "/" + fx.ProjectSlug + "/performance/",
		"/" + fx.OrgSlug + "/" + fx.ProjectSlug + "/releases/",
		"/" + fx.OrgSlug + "/" + fx.ProjectSlug + "/feedback/",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("sidebar missing %q", want)
		}
	}

	// Unknown project 404s on every T9 page.
	for _, p := range []string{"performance/", "releases/", "feedback/", "traces/" + uuid.NewString() + "/"} {
		if rec := s.get(t, "/"+fx.OrgSlug+"/no-such-project/"+p, sess, nil); rec.Code != 404 {
			t.Fatalf("GET unknown project %s = %d (want 404)", p, rec.Code)
		}
	}

	// Project creation form still CSRF-guarded after the route additions.
	rec = s.postForm(t, "/"+fx.OrgSlug+"/projects/new/",
		url.Values{"name": {"X"}}, sess)
	if rec.Code != 403 {
		t.Fatalf("CSRF-less project create = %d (want 403)", rec.Code)
	}
}
