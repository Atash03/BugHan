package httpapi

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

// apiFixture carries what issue-API tests need to hit routes and ingest data.
type apiFixture struct {
	Bearer      map[string]string
	OrgSlug     string
	ProjectSlug string
	ProjectID   string
	Key         string
	UserEmail   string
}

// apiSetup provisions account + API token + org + project through the API.
func apiSetup(t *testing.T, s *Server) *apiFixture {
	t.Helper()
	return apiSetupOrg(t, s, "Triage Org", "neo@acme.dev", "hunter2hunter2")
}

func apiSetupOrg(t *testing.T, s *Server, orgName, email, password string) *apiFixture {
	t.Helper()
	rec := s.postForm(t, "/auth/signup/", url.Values{
		"name": {"Neo"}, "email": {email}, "password": {password},
	}, jar{})
	if rec.Code != 302 {
		t.Fatalf("signup = %d %s", rec.Code, rec.Body.String())
	}
	sess, csrf := login(t, s, email, password)

	rec = s.apiCall(t, "POST", "/api/0/tokens/", `{"name":"issues"}`, sess, map[string]string{"X-CSRF-Token": csrf})
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &tok); err != nil || tok.Token == "" {
		t.Fatalf("token create = %d %s", rec.Code, rec.Body.String())
	}
	bearer := map[string]string{"Authorization": "Bearer " + tok.Token}

	rec = s.apiCall(t, "POST", "/api/0/organizations/", `{"name":"`+orgName+`"}`, jar{}, bearer)
	var org struct{ Slug string }
	if err := json.Unmarshal(rec.Body.Bytes(), &org); err != nil || org.Slug == "" {
		t.Fatalf("org create = %d %s", rec.Code, rec.Body.String())
	}

	rec = s.apiCall(t, "POST", "/api/0/organizations/"+org.Slug+"/projects/", `{"name":"Web App"}`, jar{}, bearer)
	var proj struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
		Keys []struct {
			DSN string `json:"dsn"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == "" {
		t.Fatalf("project create = %d %s", rec.Code, rec.Body.String())
	}
	at := strings.Index(proj.Keys[0].DSN, "https://") + len("https://")
	key := proj.Keys[0].DSN[at:strings.Index(proj.Keys[0].DSN, "@")]

	return &apiFixture{
		Bearer:      bearer,
		OrgSlug:     org.Slug,
		ProjectSlug: proj.Slug,
		ProjectID:   proj.ID,
		Key:         key,
		UserEmail:   email,
	}
}

func (fx *apiFixture) issuesPath() string {
	return "/api/0/projects/" + fx.OrgSlug + "/" + fx.ProjectSlug + "/issues/"
}

func (fx *apiFixture) issuePath(issueID string) string {
	return "/api/0/issues/" + issueID + "/"
}

// firstIssueID lists the project's issues and returns the first id.
func (fx *apiFixture) firstIssueID(t *testing.T, s *Server) string {
	t.Helper()
	rec := s.apiCall(t, "GET", fx.issuesPath(), "", jar{}, fx.Bearer)
	if rec.Code != 200 {
		t.Fatalf("firstIssueID list = %d: %s", rec.Code, rec.Body.String())
	}
	var got issueListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Data) == 0 {
		t.Fatalf("firstIssueID: %d %s (%v)", rec.Code, rec.Body.String(), err)
	}
	return got.Data[0]["id"].(string)
}

type issueListResponse struct {
	Data []map[string]any `json:"data"`
	Meta struct {
		Total  int `json:"total"`
		Limit  int `json:"limit"`
		Offset int `json:"offset"`
	} `json:"meta"`
}

// postErrorEnvelope sends one error event through the real ingest endpoint.
func (fx *apiFixture) postErrorEnvelope(t *testing.T, s *Server, eventID, typ, value, userID, ts string) {
	t.Helper()
	body := `{"event_id":"` + eventID + `","sent_at":"` + ts + `"}
{"type":"event"}
{"event_id":"` + eventID + `","timestamp":"` + ts + `","platform":"javascript","level":"error","exception":{"values":[{"type":"` + typ + `","value":"` + value + `"}]},"user":{"id":"` + userID + `"}}`
	rec := s.postEnvelope(t, fx.ProjectID, fx.Key, body, "")
	if rec.Code != 200 {
		t.Fatalf("envelope %s = %d: %s", eventID, rec.Code, rec.Body.String())
	}
}

// postRawEvent sends a prebuilt event payload through real ingest.
func (fx *apiFixture) postRawEvent(t *testing.T, s *Server, eventID, payload string) {
	t.Helper()
	body := `{"event_id":"` + eventID + `","sent_at":"2026-09-06T10:00:00Z"}
{"type":"event"}
` + payload
	rec := s.postEnvelope(t, fx.ProjectID, fx.Key, body, "")
	if rec.Code != 200 {
		t.Fatalf("envelope %s = %d: %s", eventID, rec.Code, rec.Body.String())
	}
}

// issueIDByTitle finds an issue id by its grouping title.
func (fx *apiFixture) issueIDByTitle(t *testing.T, s *Server, title string) string {
	t.Helper()
	rec := s.apiCall(t, "GET", fx.issuesPath(), "", jar{}, fx.Bearer)
	var got issueListResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	for _, d := range got.Data {
		if d["title"] == title {
			return d["id"].(string)
		}
	}
	t.Fatalf("issue %q not found in %d issues", title, len(got.Data))
	return ""
}

func TestAPITokenReadScope(t *testing.T) {
	s := newTestServer(t)
	fx := apiSetup(t, s)
	fx.postErrorEnvelope(t, s, "ba000000000000000000000000000001", "TypeError", "boom", "42", "2026-09-06T10:00:01Z")
	issueID := fx.firstIssueID(t, s)

	// Mint a read-scope token through the API (session caller).
	sess, csrf := login(t, s, fx.UserEmail, "hunter2hunter2")
	rec := s.apiCall(t, "POST", "/api/0/tokens/", `{"name":"ro","scope":"read"}`, sess, map[string]string{"X-CSRF-Token": csrf})
	var tok struct{ Token string }
	_ = json.Unmarshal(rec.Body.Bytes(), &tok)
	ro := map[string]string{"Authorization": "Bearer " + tok.Token}

	// Reads work.
	if rec := s.apiCall(t, "GET", fx.issuesPath(), "", jar{}, ro); rec.Code != 200 {
		t.Fatalf("read token list = %d (want 200)", rec.Code)
	}
	if rec := s.apiCall(t, "GET", fx.issuePath(issueID)+"activity/", "", jar{}, ro); rec.Code != 200 {
		t.Fatalf("read token activity = %d (want 200)", rec.Code)
	}

	// Mutations are forbidden.
	for _, m := range []struct {
		method, path, body string
	}{
		{"PUT", fx.issuePath(issueID), `{"status":"resolved"}`},
		{"POST", fx.issuePath(issueID) + "comments/", `{"body":"hi"}`},
		{"PUT", fx.issuesPath() + "bulk/", `{"ids":["` + issueID + `"],"status":"ignored"}`},
		{"POST", "/api/0/projects/" + fx.OrgSlug + "/" + fx.ProjectSlug + "/views/", `{"name":"v"}`},
		{"POST", "/api/0/projects/" + fx.OrgSlug + "/" + fx.ProjectSlug + "/delete-data/", ""},
	} {
		if rec := s.apiCall(t, m.method, m.path, m.body, jar{}, ro); rec.Code != 403 {
			t.Fatalf("read token %s %s = %d (want 403)", m.method, m.path, rec.Code)
		}
	}

	// The write-scope token still mutates fine.
	if rec := s.apiCall(t, "PUT", fx.issuePath(issueID), `{"status":"resolved"}`, jar{}, fx.Bearer); rec.Code != 200 {
		t.Fatalf("write token = %d (want 200)", rec.Code)
	}
}

func TestProjectDeleteAllEventData(t *testing.T) {
	s := newTestServer(t)
	fx := apiSetup(t, s)

	// Victim project gets events, issues, feedback, a saved view.
	fx.postErrorEnvelope(t, s, "b9000000000000000000000000000001", "TypeError", "boom", "42", "2026-09-06T10:00:01Z")
	s.postEnvelope(t, fx.ProjectID, fx.Key, `{"event_id":"d5f8d0e2a2c14d8e9f1a2b3c4d5e6f70","sent_at":"2026-09-06T10:00:03.000Z"}
{"type":"feedback"}
{"event_id":"d5f8d0e2a2c14d8e9f1a2b3c4d5e6f70","timestamp":"2026-09-06T10:00:03.000Z","platform":"javascript","contexts":{"feedback":{"message":"meh","contact_email":"end@user.io"}}}`, "")
	viewsPath := "/api/0/projects/" + fx.OrgSlug + "/" + fx.ProjectSlug + "/views/"
	if rec := s.apiCall(t, "POST", viewsPath, `{"name":"keep me"}`, jar{}, fx.Bearer); rec.Code != 201 {
		t.Fatalf("view create = %d", rec.Code)
	}

	// Sibling project in the same org stays untouched.
	siblingRec := s.apiCall(t, "POST", "/api/0/organizations/"+fx.OrgSlug+"/projects/", `{"name":"Other App"}`, jar{}, fx.Bearer)
	var sibling struct {
		ID   string                 `json:"id"`
		Slug string                 `json:"slug"`
		Keys []struct{ DSN string } `json:"keys"`
	}
	_ = json.Unmarshal(siblingRec.Body.Bytes(), &sibling)
	at := strings.Index(sibling.Keys[0].DSN, "https://") + len("https://")
	sibKey := sibling.Keys[0].DSN[at:strings.Index(sibling.Keys[0].DSN, "@")]
	fx2 := &apiFixture{Bearer: fx.Bearer, OrgSlug: fx.OrgSlug, ProjectSlug: sibling.Slug, ProjectID: sibling.ID, Key: sibKey, UserEmail: fx.UserEmail}
	fx2.postErrorEnvelope(t, s, "b9000000000000000000000000000002", "TypeError", "sibling", "42", "2026-09-06T10:00:02Z")

	deletePath := "/api/0/projects/" + fx.OrgSlug + "/" + fx.ProjectSlug + "/delete-data/"

	// Unauthenticated → 401.
	if rec := s.apiCall(t, "POST", deletePath, "", jar{}, nil); rec.Code != 401 {
		t.Fatalf("unauthenticated = %d (want 401)", rec.Code)
	}

	// A plain member → 403 (destructive action is admin-only).
	fx.addMember(t, s, "trixie@acme.dev")
	tsess, tcsrf := login(t, s, "trixie@acme.dev", "hunter2hunter2")
	trixieToken := s.apiCall(t, "POST", "/api/0/tokens/", `{"name":"t2"}`, tsess, map[string]string{"X-CSRF-Token": tcsrf})
	var tt struct{ Token string }
	_ = json.Unmarshal(trixieToken.Body.Bytes(), &tt)
	trixieBearer := map[string]string{"Authorization": "Bearer " + tt.Token}
	if rec := s.apiCall(t, "POST", deletePath, "", jar{}, trixieBearer); rec.Code != 403 {
		t.Fatalf("member delete = %d (want 403)", rec.Code)
	}

	// Owner wipes project A.
	if rec := s.apiCall(t, "POST", deletePath, "", jar{}, fx.Bearer); rec.Code != 204 {
		t.Fatalf("delete-data = %d: %s", rec.Code, rec.Body.String())
	}

	count := func(table, projectID string) int {
		t.Helper()
		var n int
		if err := s.pool.QueryRow(t.Context(),
			`SELECT count(*) FROM `+table+` WHERE project_id = $1`, projectID).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}
	if n := count("events_part", fx.ProjectID); n != 0 {
		t.Fatalf("events left = %d", n)
	}
	if n := count("feedbacks", fx.ProjectID); n != 0 {
		t.Fatalf("feedback left = %d", n)
	}
	if n := count("issues", fx.ProjectID); n != 0 {
		t.Fatalf("issues left = %d", n)
	}
	if n := count("project_event_rollups", fx.ProjectID); n != 0 {
		t.Fatalf("project rollups left = %d", n)
	}

	// Sibling project B keeps its data.
	if n := count("issues", sibling.ID); n != 1 {
		t.Fatalf("sibling issues = %d (want 1)", n)
	}

	// Saved views survive the wipe.
	rec := s.apiCall(t, "GET", viewsPath, "", jar{}, fx.Bearer)
	var views []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &views)
	if len(views) != 1 || views[0]["name"] != "keep me" {
		t.Fatalf("views after wipe = %v", views)
	}
}

func TestBulkUpdateRecordsActivity(t *testing.T) {
	s := newTestServer(t)
	fx := apiSetup(t, s)
	fx.postErrorEnvelope(t, s, "b8000000000000000000000000000001", "TypeError", "one", "42", "2026-09-06T10:00:01Z")
	fx.postErrorEnvelope(t, s, "b8000000000000000000000000000002", "ReferenceError", "two", "42", "2026-09-06T10:00:02Z")

	rec := s.apiCall(t, "GET", fx.issuesPath(), "", jar{}, fx.Bearer)
	var all issueListResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &all)
	id := all.Data[0]["id"].(string)

	if rec := s.apiCall(t, "PUT", fx.issuesPath()+"bulk/", `{"ids":["`+id+`"],"status":"resolved"}`, jar{}, fx.Bearer); rec.Code != 200 {
		t.Fatalf("bulk = %d", rec.Code)
	}

	// The bulk action landed on the issue's activity feed.
	rec = s.apiCall(t, "GET", fx.issuePath(id)+"activity/", "", jar{}, fx.Bearer)
	var feed []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &feed)
	if len(feed) != 1 || feed[0]["type"] != "status" {
		t.Fatalf("feed = %v", feed)
	}
	if st, ok := feed[0]["data"].(map[string]any); !ok || st["to"] != "resolved" {
		t.Fatalf("status data = %v", feed[0]["data"])
	}
	if feed[0]["author"] == nil {
		t.Fatalf("bulk activity must carry the actor")
	}
}

func TestSavedViewsCRUD(t *testing.T) {
	s := newTestServer(t)
	fx := apiSetup(t, s)
	viewsPath := "/api/0/projects/" + fx.OrgSlug + "/" + fx.ProjectSlug + "/views/"

	// Create.
	rec := s.apiCall(t, "POST", viewsPath, `{"name":"My unresolved","query":"status=unresolved&query=timeout"}`, jar{}, fx.Bearer)
	if rec.Code != 201 {
		t.Fatalf("view create = %d: %s", rec.Code, rec.Body.String())
	}
	var view map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	if view["name"] != "My unresolved" || view["query"] != "status=unresolved&query=timeout" || view["id"] == "" {
		t.Fatalf("view = %v", view)
	}
	if author, ok := view["created_by"].(map[string]any); !ok || author["email"] != fx.UserEmail {
		t.Fatalf("created_by = %v", view["created_by"])
	}

	// List shows it.
	rec = s.apiCall(t, "GET", viewsPath, "", jar{}, fx.Bearer)
	var views []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &views)
	if len(views) != 1 || views[0]["name"] != "My unresolved" {
		t.Fatalf("views = %v", views)
	}

	// Duplicate name → 400.
	if rec := s.apiCall(t, "POST", viewsPath, `{"name":"My unresolved","query":""}`, jar{}, fx.Bearer); rec.Code != 400 {
		t.Fatalf("duplicate name = %d (want 400)", rec.Code)
	}
	// Empty name → 400.
	if rec := s.apiCall(t, "POST", viewsPath, `{"name":"  ","query":""}`, jar{}, fx.Bearer); rec.Code != 400 {
		t.Fatalf("empty name = %d (want 400)", rec.Code)
	}

	// Delete.
	id := view["id"].(string)
	if rec := s.apiCall(t, "DELETE", viewsPath+id+"/", "", jar{}, fx.Bearer); rec.Code != 204 {
		t.Fatalf("delete = %d (want 204)", rec.Code)
	}
	// Gone from the list.
	rec = s.apiCall(t, "GET", viewsPath, "", jar{}, fx.Bearer)
	_ = json.Unmarshal(rec.Body.Bytes(), &views)
	if len(views) != 0 {
		t.Fatalf("views after delete = %v", views)
	}
	// Unknown view → 404.
	if rec := s.apiCall(t, "DELETE", viewsPath+"00000000-0000-0000-0000-000000000000/", "", jar{}, fx.Bearer); rec.Code != 404 {
		t.Fatalf("unknown view = %d (want 404)", rec.Code)
	}
}

func TestIssueCommentsAndActivity(t *testing.T) {
	s := newTestServer(t)
	fx := apiSetup(t, s)
	fx.postErrorEnvelope(t, s, "b7000000000000000000000000000001", "TypeError", "boom", "42", "2026-09-06T10:00:01Z")
	issueID := fx.firstIssueID(t, s)

	// Add a comment.
	rec := s.apiCall(t, "POST", fx.issuePath(issueID)+"comments/", `{"body":"looks fixed to me"}`, jar{}, fx.Bearer)
	if rec.Code != 201 {
		t.Fatalf("comment = %d: %s", rec.Code, rec.Body.String())
	}
	var note map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &note)
	if note["type"] != "note" || note["body"] != "looks fixed to me" {
		t.Fatalf("note = %v", note)
	}
	author := note["author"].(map[string]any)
	if author["email"] != fx.UserEmail {
		t.Fatalf("note author = %v", author)
	}

	// Empty body → 400.
	if rec := s.apiCall(t, "POST", fx.issuePath(issueID)+"comments/", `{"body":"  "}`, jar{}, fx.Bearer); rec.Code != 400 {
		t.Fatalf("empty comment = %d (want 400)", rec.Code)
	}

	// Triage actions are recorded as activity too.
	if rec := s.apiCall(t, "PUT", fx.issuePath(issueID), `{"status":"resolved"}`, jar{}, fx.Bearer); rec.Code != 200 {
		t.Fatalf("resolve = %d", rec.Code)
	}
	if rec := s.apiCall(t, "PUT", fx.issuePath(issueID), `{"assignedTo":"me"}`, jar{}, fx.Bearer); rec.Code != 200 {
		t.Fatalf("assign = %d", rec.Code)
	}

	// Activity feed: newest first — assignment, status change, note.
	rec = s.apiCall(t, "GET", fx.issuePath(issueID)+"activity/", "", jar{}, fx.Bearer)
	if rec.Code != 200 {
		t.Fatalf("activity = %d: %s", rec.Code, rec.Body.String())
	}
	var feed []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &feed)
	if len(feed) != 3 {
		t.Fatalf("feed length = %d (want 3): %v", len(feed), feed)
	}
	if feed[0]["type"] != "assignment" {
		t.Fatalf("feed[0] = %v", feed[0])
	}
	if feed[1]["type"] != "status" {
		t.Fatalf("feed[1] = %v", feed[1])
	}
	if st, ok := feed[1]["data"].(map[string]any); !ok || st["from"] != "unresolved" || st["to"] != "resolved" {
		t.Fatalf("status data = %v", feed[1]["data"])
	}
	if feed[2]["type"] != "note" || feed[2]["body"] != "looks fixed to me" {
		t.Fatalf("feed[2] = %v", feed[2])
	}
	if a := feed[2]["author"].(map[string]any); a["email"] != fx.UserEmail {
		t.Fatalf("feed author = %v", a)
	}
}

func TestBulkIssueUpdate(t *testing.T) {
	s := newTestServer(t)
	fx := apiSetup(t, s)
	fx.postErrorEnvelope(t, s, "b6000000000000000000000000000001", "TypeError", "one", "42", "2026-09-06T10:00:01Z")
	fx.postErrorEnvelope(t, s, "b6000000000000000000000000000002", "ReferenceError", "two", "42", "2026-09-06T10:00:02Z")
	fx.postErrorEnvelope(t, s, "b6000000000000000000000000000003", "SyntaxError", "three", "42", "2026-09-06T10:00:03Z")

	rec := s.apiCall(t, "GET", fx.issuesPath(), "", jar{}, fx.Bearer)
	var all issueListResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &all)
	ids := []string{all.Data[0]["id"].(string), all.Data[1]["id"].(string)}

	// Bulk resolve two issues.
	rec = s.apiCall(t, "PUT", fx.issuesPath()+"bulk/", `{"ids":["`+ids[0]+`","`+ids[1]+`"],"status":"resolved"}`, jar{}, fx.Bearer)
	if rec.Code != 200 {
		t.Fatalf("bulk = %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Updated int `json:"updated"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Updated != 2 {
		t.Fatalf("updated = %d (want 2)", got.Updated)
	}
	rec = s.apiCall(t, "GET", fx.issuesPath()+"?status=resolved", "", jar{}, fx.Bearer)
	_ = json.Unmarshal(rec.Body.Bytes(), &all)
	if len(all.Data) != 2 {
		t.Fatalf("resolved after bulk = %d (want 2)", len(all.Data))
	}

	// Bulk assign one.
	rec = s.apiCall(t, "PUT", fx.issuesPath()+"bulk/", `{"ids":["`+ids[0]+`"],"assignedTo":"me"}`, jar{}, fx.Bearer)
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Updated != 1 {
		t.Fatalf("assigned = %d (want 1)", got.Updated)
	}

	// Unknown ids are skipped, not fatal.
	rec = s.apiCall(t, "PUT", fx.issuesPath()+"bulk/", `{"ids":["`+ids[0]+`","00000000-0000-0000-0000-000000000000"],"status":"ignored"}`, jar{}, fx.Bearer)
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != 200 || got.Updated != 1 {
		t.Fatalf("unknown id = %d %+v", rec.Code, got)
	}

	// Invalid status → 400.
	rec = s.apiCall(t, "PUT", fx.issuesPath()+"bulk/", `{"ids":["`+ids[0]+`"],"status":"bogus"}`, jar{}, fx.Bearer)
	if rec.Code != 400 {
		t.Fatalf("bogus status = %d (want 400)", rec.Code)
	}

	// Empty ids → 400.
	rec = s.apiCall(t, "PUT", fx.issuesPath()+"bulk/", `{"ids":[],"status":"resolved"}`, jar{}, fx.Bearer)
	if rec.Code != 400 {
		t.Fatalf("empty ids = %d (want 400)", rec.Code)
	}
}

func TestEventDetail(t *testing.T) {
	s := newTestServer(t)
	fx := apiSetup(t, s)
	payload := `{"timestamp":"2026-09-06T10:00:01Z","platform":"javascript","level":"error","message":"boom happened","exception":{"values":[{"type":"TypeError","value":"boom"}]},"breadcrumbs":[{"timestamp":1788220800,"category":"ui","type":"user","message":"clicked save"}],"contexts":{"trace":{"trace_id":"743ad8bbfdd84e99bc38b4729e2864de","span_id":"a0cfbde2bdff3adc"},"react":{"component_name":"SaveButton"}},"tags":{"browser":"Chrome"}}`
	fx.postRawEvent(t, s, "b5000000000000000000000000000001", payload)
	issueID := fx.firstIssueID(t, s)

	// Find the event id from the issue's event list.
	rec := s.apiCall(t, "GET", fx.issuePath(issueID)+"events/", "", jar{}, fx.Bearer)
	var list issueListResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	eventID := list.Data[0]["id"].(string)

	rec = s.apiCall(t, "GET", fx.issuePath(issueID)+"events/"+eventID+"/", "", jar{}, fx.Bearer)
	if rec.Code != 200 {
		t.Fatalf("event detail = %d: %s", rec.Code, rec.Body.String())
	}
	var ev map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &ev); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if ev["id"] != eventID || ev["issue_id"] != issueID || ev["message"] != "boom happened" {
		t.Fatalf("event identity = %v/%v/%v", ev["id"], ev["issue_id"], ev["message"])
	}

	// Full raw payload is present.
	pl, ok := ev["payload"].(map[string]any)
	if !ok || len(pl) == 0 {
		t.Fatalf("payload missing: %v", ev["payload"])
	}

	// Breadcrumbs extracted from the payload.
	bc, ok := ev["breadcrumbs"].([]any)
	if !ok || len(bc) != 1 || bc[0].(map[string]any)["message"] != "clicked save" {
		t.Fatalf("breadcrumbs = %v", ev["breadcrumbs"])
	}

	// Contexts (beyond trace) extracted.
	ctxAny, ok := ev["contexts"].(map[string]any)
	if !ok {
		t.Fatalf("contexts = %v", ev["contexts"])
	}
	react, ok := ctxAny["react"].(map[string]any)
	if !ok || react["component_name"] != "SaveButton" {
		t.Fatalf("react context = %v", ctxAny["react"])
	}

	// Tags as a map.
	tags, ok := ev["tags"].(map[string]any)
	if !ok || tags["browser"] != "Chrome" {
		t.Fatalf("tags = %v", ev["tags"])
	}

	// Unknown event → 404.
	if rec := s.apiCall(t, "GET", fx.issuePath(issueID)+"events/00000000-0000-0000-0000-000000000000/", "", jar{}, fx.Bearer); rec.Code != 404 {
		t.Fatalf("unknown event = %d (want 404)", rec.Code)
	}
}

func TestIssueDetailAndEvents(t *testing.T) {
	s := newTestServer(t)
	fx := apiSetup(t, s)
	fx.postErrorEnvelope(t, s, "b4000000000000000000000000000001", "TypeError", "boom", "42", "2026-09-06T10:00:01Z")
	fx.postErrorEnvelope(t, s, "b4000000000000000000000000000002", "TypeError", "boom", "42", "2026-09-06T10:00:02Z")
	issueID := fx.firstIssueID(t, s)

	// Detail mirrors the list row shape.
	rec := s.apiCall(t, "GET", fx.issuePath(issueID), "", jar{}, fx.Bearer)
	if rec.Code != 200 {
		t.Fatalf("detail = %d: %s", rec.Code, rec.Body.String())
	}
	var detail map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &detail)
	if detail["id"] != issueID || detail["count"] != float64(2) {
		t.Fatalf("detail = %v / count %v", detail["id"], detail["count"])
	}

	// Events: newest first, list shape without payload.
	rec = s.apiCall(t, "GET", fx.issuePath(issueID)+"events/", "", jar{}, fx.Bearer)
	if rec.Code != 200 {
		t.Fatalf("events = %d: %s", rec.Code, rec.Body.String())
	}
	var got issueListResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Meta.Total != 2 || len(got.Data) != 2 {
		t.Fatalf("events meta = %+v", got.Meta)
	}
	first := got.Data[0]
	if first["level"] != "error" || first["environment"] != "production" || first["issue_id"] != issueID {
		t.Fatalf("event row = %+v", first)
	}
	if first["timestamp"] == "" || first["id"] == "" {
		t.Fatalf("event identity missing: %+v", first)
	}
	if _, hasPayload := first["payload"]; hasPayload {
		t.Fatalf("event list must not carry payload: %+v", first)
	}

	// Unknown issue → 404.
	if rec := s.apiCall(t, "GET", fx.issuePath("00000000-0000-0000-0000-000000000000")+"events/", "", jar{}, fx.Bearer); rec.Code != 404 {
		t.Fatalf("unknown issue events = %d (want 404)", rec.Code)
	}

	// A member of another org cannot see the issue: 404, not 403 (no leak).
	other := apiSetupOrg(t, s, "Rival Org", "rival@acme.dev", "hunter2hunter2")
	if rec := s.apiCall(t, "GET", fx.issuePath(issueID), "", jar{}, other.Bearer); rec.Code != 404 {
		t.Fatalf("cross-org detail = %d (want 404)", rec.Code)
	}
}

func TestProjectTagFacets(t *testing.T) {
	s := newTestServer(t)
	fx := apiSetup(t, s)

	tagged := func(typ, value, tags, environment, ts string) string {
		return `{"timestamp":"2026-09-06T` + ts + `","platform":"javascript","level":"error","environment":"` + environment + `","exception":{"values":[{"type":"` + typ + `","value":"` + value + `"}]},"tags":` + tags + `}`
	}
	// Two events fold into one issue: Chrome/macOS counts one distinct issue.
	fx.postRawEvent(t, s, "b3000000000000000000000000000001", tagged("TypeError", "chrome mac", `{"browser":"Chrome","os":"macOS"}`, "production", "10:00:01Z"))
	fx.postRawEvent(t, s, "b3000000000000000000000000000002", tagged("TypeError", "chrome mac", `{"browser":"Chrome","os":"macOS"}`, "production", "10:00:02Z"))
	fx.postRawEvent(t, s, "b3000000000000000000000000000003", tagged("ReferenceError", "chrome win", `{"browser":"Chrome","os":"Windows"}`, "production", "10:00:03Z"))
	fx.postRawEvent(t, s, "b3000000000000000000000000000004", tagged("SyntaxError", "firefox mac", `{"browser":"Firefox","os":"macOS"}`, "staging", "10:00:04Z"))

	fetch := func(query string) []map[string]any {
		t.Helper()
		rec := s.apiCall(t, "GET", "/api/0/projects/"+fx.OrgSlug+"/"+fx.ProjectSlug+"/tags/"+query, "", jar{}, fx.Bearer)
		if rec.Code != 200 {
			t.Fatalf("tags %q = %d: %s", query, rec.Code, rec.Body.String())
		}
		var got []map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		return got
	}

	facets := fetch("")
	if len(facets) != 2 || facets[0]["key"] != "browser" || facets[1]["key"] != "os" {
		t.Fatalf("facets keys = %+v", facets)
	}
	browser := facets[0]["values"].([]any)
	if len(browser) != 2 {
		t.Fatalf("browser values = %+v", browser)
	}
	if browser[0].(map[string]any)["value"] != "Chrome" || browser[0].(map[string]any)["count"] != float64(2) {
		t.Fatalf("browser[0] = %+v (Chrome=2 distinct issues)", browser[0])
	}
	if browser[1].(map[string]any)["value"] != "Firefox" || browser[1].(map[string]any)["count"] != float64(1) {
		t.Fatalf("browser[1] = %+v", browser[1])
	}
	osFacet := facets[1]["values"].([]any)
	if osFacet[0].(map[string]any)["value"] != "macOS" || osFacet[0].(map[string]any)["count"] != float64(2) {
		t.Fatalf("os[0] = %+v", osFacet[0])
	}

	// Facets respect the issue filters (staging issue excluded).
	facets = fetch("?environment=production")
	browser = facets[0]["values"].([]any)
	if len(browser) != 1 || browser[0].(map[string]any)["value"] != "Chrome" {
		t.Fatalf("production browser facet = %+v", browser)
	}

	// Unauthenticated → 401.
	if rec := s.get(t, "/api/0/projects/"+fx.OrgSlug+"/"+fx.ProjectSlug+"/tags/", jar{}, nil); rec.Code != 401 {
		t.Fatalf("unauthenticated tags = %d (want 401)", rec.Code)
	}
}

func TestIssuesListTextSearch(t *testing.T) {
	s := newTestServer(t)
	fx := apiSetup(t, s)

	fx.postErrorEnvelope(t, s, "b2000000000000000000000000000001", "TimeoutError", "database timeout while pooling", "42", "2026-09-06T10:00:01Z")
	fx.postErrorEnvelope(t, s, "b2000000000000000000000000000002", "FetchError", "fetch failed", "42", "2026-09-06T10:00:02Z")
	fx.postErrorEnvelope(t, s, "b2000000000000000000000000000003", "TypeError", "undefined is not a function", "42", "2026-09-06T10:00:03Z")

	list := func(query string) issueListResponse {
		t.Helper()
		rec := s.apiCall(t, "GET", fx.issuesPath()+query, "", jar{}, fx.Bearer)
		if rec.Code != 200 {
			t.Fatalf("list %q = %d: %s", query, rec.Code, rec.Body.String())
		}
		var got issueListResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		return got
	}

	// Title substring, case-insensitive.
	if got := titles(list("?query=TIMEOUT")); got != "TimeoutError: database timeout while pooling" {
		t.Fatalf("query=TIMEOUT = %q", got)
	}
	// No match → empty.
	if got := titles(list("?query=nonexistent")); got != "" {
		t.Fatalf("query=nonexistent = %q", got)
	}
	// Query composes with other filters.
	if got := titles(list("?query=timeout&level=error")); got != "TimeoutError: database timeout while pooling" {
		t.Fatalf("combined = %q", got)
	}
	// % in the query is literal, not a wildcard: matches nothing.
	if got := titles(list("?query=100%25")); got != "" {
		t.Fatalf("query=100%% = %q", got)
	}
}

func TestIssuesListTagFilter(t *testing.T) {
	s := newTestServer(t)
	fx := apiSetup(t, s)

	tagged := func(typ, value, tags, ts string) string {
		return `{"timestamp":"2026-09-06T` + ts + `","platform":"javascript","level":"error","exception":{"values":[{"type":"` + typ + `","value":"` + value + `"}]},"tags":` + tags + `}`
	}
	fx.postRawEvent(t, s, "b1000000000000000000000000000001", tagged("TypeError", "chrome mac", `{"browser":"Chrome","os":"macOS"}`, "10:00:01Z"))
	fx.postRawEvent(t, s, "b1000000000000000000000000000002", tagged("ReferenceError", "chrome win", `{"browser":"Chrome","os":"Windows"}`, "10:00:02Z"))
	fx.postRawEvent(t, s, "b1000000000000000000000000000003", tagged("SyntaxError", "firefox mac", `{"browser":"Firefox","os":"macOS"}`, "10:00:03Z"))
	// Value containing a colon: only the first colon splits key from value.
	fx.postRawEvent(t, s, "b1000000000000000000000000000004", tagged("RangeError", "chrome mobile", `{"browser":"Chrome:Mobile"}`, "10:00:04Z"))

	list := func(query string) issueListResponse {
		t.Helper()
		rec := s.apiCall(t, "GET", fx.issuesPath()+query, "", jar{}, fx.Bearer)
		if rec.Code != 200 {
			t.Fatalf("list %q = %d: %s", query, rec.Code, rec.Body.String())
		}
		var got issueListResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		return got
	}

	cases := []struct {
		query string
		want  string
	}{
		{"?tag=browser:Chrome", "ReferenceError: chrome win|TypeError: chrome mac"}, // exact value: Chrome:Mobile does not match,
		{"?tag=browser:Chrome&tag=os:Windows", "ReferenceError: chrome win"},
		{"?tag=os:macOS", "SyntaxError: firefox mac|TypeError: chrome mac"},
		{"?tag=browser:Chrome:Mobile", "RangeError: chrome mobile"},
		{"?tag=browser:Opera", ""},
	}
	for _, tc := range cases {
		if got := titles(list(tc.query)); got != tc.want {
			t.Fatalf("%s = %q (want %q)", tc.query, got, tc.want)
		}
	}
}

func TestIssuesListFilters(t *testing.T) {
	s := newTestServer(t)
	fx := apiSetup(t, s)

	env := func(typ, value, level, environment, release, ts string) string {
		return `{"timestamp":"2026-09-06T` + ts + `","platform":"javascript","level":"` + level + `","environment":"` + environment + `","release":"` + release + `","exception":{"values":[{"type":"` + typ + `","value":"` + value + `"}]}}`
	}
	fx.postRawEvent(t, s, "af000000000000000000000000000001", env("TypeError", "db down", "error", "production", "app@1.0.0", "10:00:01Z"))
	fx.postRawEvent(t, s, "af000000000000000000000000000002", env("ReferenceError", "cart missing", "warning", "staging", "app@2.0.0", "10:00:02Z"))
	fx.postRawEvent(t, s, "af000000000000000000000000000003", env("SyntaxError", "parse fail", "error", "production", "app@1.0.0", "10:00:03Z"))

	aID := fx.issueIDByTitle(t, s, "TypeError: db down")
	if rec := s.apiCall(t, "PUT", fx.issuePath(aID), `{"status":"resolved"}`, jar{}, fx.Bearer); rec.Code != 200 {
		t.Fatalf("resolve = %d", rec.Code)
	}

	list := func(query string) issueListResponse {
		t.Helper()
		rec := s.apiCall(t, "GET", fx.issuesPath()+query, "", jar{}, fx.Bearer)
		if rec.Code != 200 {
			t.Fatalf("list %q = %d: %s", query, rec.Code, rec.Body.String())
		}
		var got issueListResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		return got
	}

	cases := []struct {
		query string
		want  string
	}{
		{"?status=resolved", "TypeError: db down"},
		{"?status=unresolved", "SyntaxError: parse fail|ReferenceError: cart missing"},
		{"?level=warning", "ReferenceError: cart missing"},
		{"?environment=staging", "ReferenceError: cart missing"},
		{"?release=app@2.0.0", "ReferenceError: cart missing"},
		{"?environment=production&level=error", "SyntaxError: parse fail|TypeError: db down"},
		{"?status=unresolved&environment=production", "SyntaxError: parse fail"},
		{"?environment=production&environment=staging", "SyntaxError: parse fail|ReferenceError: cart missing|TypeError: db down"},
		{"?status=ignored", ""},
	}
	for _, tc := range cases {
		if got := titles(list(tc.query)); got != tc.want {
			t.Fatalf("%s = %q (want %q)", tc.query, got, tc.want)
		}
	}
}

func TestIssuesListPagingAndSort(t *testing.T) {
	s := newTestServer(t)
	fx := apiSetup(t, s)

	// Counts inverse to recency so default (last_seen) and sort=count order differ:
	// default: TypeError, ReferenceError, SyntaxError · by count: 3, 2, 1.
	post := func(eventID, typ, value, ts string) {
		fx.postErrorEnvelope(t, s, eventID, typ, value, "42", ts)
	}
	post("b1000000000000000000000000000001", "TypeError", "one at a time", "2026-09-06T10:00:09Z")
	post("b1000000000000000000000000000002", "ReferenceError", "twice", "2026-09-06T10:00:03Z")
	post("b1000000000000000000000000000003", "ReferenceError", "twice", "2026-09-06T10:00:08Z")
	post("b1000000000000000000000000000004", "SyntaxError", "thrice", "2026-09-06T10:00:01Z")
	post("ab000000000000000000000000000005", "SyntaxError", "thrice", "2026-09-06T10:00:05Z")
	post("ab000000000000000000000000000006", "SyntaxError", "thrice", "2026-09-06T10:00:07Z")

	list := func(query string) issueListResponse {
		t.Helper()
		rec := s.apiCall(t, "GET", fx.issuesPath()+query, "", jar{}, fx.Bearer)
		if rec.Code != 200 {
			t.Fatalf("list %q = %d: %s", query, rec.Code, rec.Body.String())
		}
		var got issueListResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("bad JSON: %v", err)
		}
		return got
	}

	// Default order: last_seen desc.
	got := list("")
	if titles(got) != "TypeError: one at a time|ReferenceError: twice|SyntaxError: thrice" {
		t.Fatalf("default order = %s", titles(got))
	}

	// Sort by count desc.
	got = list("?sort=count")
	if titles(got) != "SyntaxError: thrice|ReferenceError: twice|TypeError: one at a time" {
		t.Fatalf("sort=count order = %s", titles(got))
	}

	// Paging: limit + offset with a stable total.
	got = list("?sort=count&limit=2")
	if len(got.Data) != 2 || got.Meta.Total != 3 || got.Meta.Limit != 2 || got.Meta.Offset != 0 {
		t.Fatalf("limit=2 meta = %+v len=%d", got.Meta, len(got.Data))
	}
	got = list("?sort=count&limit=2&offset=2")
	if len(got.Data) != 1 || got.Meta.Offset != 2 || got.Data[0]["title"] != "TypeError: one at a time" {
		t.Fatalf("offset=2 = %s", titles(got))
	}

	// limit clamps to 100.
	if got = list("?limit=500"); got.Meta.Limit != 100 {
		t.Fatalf("limit=500 → %d (want 100)", got.Meta.Limit)
	}

	// Unknown sort → 400.
	if rec := s.apiCall(t, "GET", fx.issuesPath()+"?sort=bogus", "", jar{}, fx.Bearer); rec.Code != 400 {
		t.Fatalf("sort=bogus = %d (want 400)", rec.Code)
	}
}

// titles joins the list's titles for a readable order assertion.
func titles(got issueListResponse) string {
	parts := []string{}
	for _, d := range got.Data {
		parts = append(parts, d["title"].(string))
	}
	return strings.Join(parts, "|")
}

func TestIssueUpdateStatus(t *testing.T) {
	s := newTestServer(t)
	fx := apiSetup(t, s)
	fx.postErrorEnvelope(t, s, "ac000000000000000000000000000001", "TypeError", "boom", "42", "2026-09-06T10:00:00Z")
	issueID := fx.firstIssueID(t, s)

	// Resolve.
	rec := s.apiCall(t, "PUT", fx.issuePath(issueID), `{"status":"resolved"}`, jar{}, fx.Bearer)
	if rec.Code != 200 {
		t.Fatalf("resolve = %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["status"] != "resolved" || got["substatus"] != "" {
		t.Fatalf("resolved issue = %v/%v", got["status"], got["substatus"])
	}

	// Ignore.
	rec = s.apiCall(t, "PUT", fx.issuePath(issueID), `{"status":"ignored"}`, jar{}, fx.Bearer)
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["status"] != "ignored" {
		t.Fatalf("ignored issue = %v", got["status"])
	}

	// Back to unresolved (clears substatus).
	rec = s.apiCall(t, "PUT", fx.issuePath(issueID), `{"status":"unresolved"}`, jar{}, fx.Bearer)
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["status"] != "unresolved" || got["substatus"] != "" {
		t.Fatalf("unresolved issue = %v/%v", got["status"], got["substatus"])
	}

	// Invalid status → 400.
	if rec := s.apiCall(t, "PUT", fx.issuePath(issueID), `{"status":"bogus"}`, jar{}, fx.Bearer); rec.Code != 400 {
		t.Fatalf("bogus status = %d (want 400)", rec.Code)
	}

	// Unknown issue → 404.
	if rec := s.apiCall(t, "PUT", "/api/0/issues/00000000-0000-0000-0000-000000000000/", `{"status":"resolved"}`, jar{}, fx.Bearer); rec.Code != 404 {
		t.Fatalf("unknown issue = %d (want 404)", rec.Code)
	}

	// Unauthenticated → 401.
	if rec := s.apiCall(t, "PUT", fx.issuePath(issueID), `{"status":"resolved"}`, jar{}, nil); rec.Code != 401 {
		t.Fatalf("unauthenticated = %d (want 401)", rec.Code)
	}
}

func TestIssueRegressionSurfacesInList(t *testing.T) {
	s := newTestServer(t)
	fx := apiSetup(t, s)
	fx.postErrorEnvelope(t, s, "ad000000000000000000000000000001", "TypeError", "boom", "42", "2026-09-06T10:00:00Z")
	issueID := fx.firstIssueID(t, s)

	if rec := s.apiCall(t, "PUT", fx.issuePath(issueID), `{"status":"resolved"}`, jar{}, fx.Bearer); rec.Code != 200 {
		t.Fatalf("resolve = %d", rec.Code)
	}

	// A new matching event regresses the resolved issue (T4 pipeline).
	fx.postErrorEnvelope(t, s, "ad000000000000000000000000000002", "TypeError", "boom", "42", "2026-09-06T10:00:05Z")

	rec := s.apiCall(t, "GET", fx.issuesPath(), "", jar{}, fx.Bearer)
	var got issueListResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Data) != 1 {
		t.Fatalf("want 1 issue, got %d", len(got.Data))
	}
	row := got.Data[0]
	if row["status"] != "unresolved" || row["substatus"] != "regressed" {
		t.Fatalf("regression = %v/%v (want unresolved/regressed)", row["status"], row["substatus"])
	}
	if row["count"] != float64(2) {
		t.Fatalf("count = %v", row["count"])
	}
}

// addMember signs a new user up, invites them to the org as a member, accepts
// the invite, and returns their user id.
func (fx *apiFixture) addMember(t *testing.T, s *Server, email string) string {
	t.Helper()
	rec := s.postForm(t, "/auth/signup/", url.Values{
		"name": {"Trixie"}, "email": {email}, "password": {"hunter2hunter2"},
	}, jar{})
	if rec.Code != 302 {
		t.Fatalf("signup = %d %s", rec.Code, rec.Body.String())
	}
	tj := captureCookies(rec)
	tcsrf := extractCSRF(s.get(t, "/auth/landing/", tj, nil).Body.String())

	rec = s.apiCall(t, "POST", "/api/0/organizations/"+fx.OrgSlug+"/invites/",
		`{"email":"`+email+`","role":"member"}`, jar{}, fx.Bearer)
	var inv struct {
		InviteLink string `json:"invite_link"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &inv)
	if inv.InviteLink == "" {
		t.Fatalf("no invite link: %s", rec.Body.String())
	}
	token := inv.InviteLink[strings.LastIndex(inv.InviteLink, "/")+1:]
	if rec := s.postForm(t, "/accept/"+token, url.Values{"csrf_token": {tcsrf}}, tj); rec.Code != 302 {
		t.Fatalf("accept = %d %s", rec.Code, rec.Body.String())
	}

	rec = s.apiCall(t, "GET", "/api/0/organizations/"+fx.OrgSlug+"/members/", "", jar{}, fx.Bearer)
	var members []struct {
		UserID string `json:"user_id"`
		Email  string `json:"email"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &members); err != nil {
		t.Fatalf("members = %d %s", rec.Code, rec.Body.String())
	}
	for _, m := range members {
		if m.Email == email {
			return m.UserID
		}
	}
	t.Fatalf("member %s missing after accept", email)
	return ""
}

func TestIssueAssign(t *testing.T) {
	s := newTestServer(t)
	fx := apiSetup(t, s)
	trixieID := fx.addMember(t, s, "trixie@acme.dev")
	fx.postErrorEnvelope(t, s, "ae000000000000000000000000000001", "TypeError", "boom", "42", "2026-09-06T10:00:00Z")
	issueID := fx.firstIssueID(t, s)

	put := func(body string) (int, map[string]any) {
		t.Helper()
		rec := s.apiCall(t, "PUT", fx.issuePath(issueID), body, jar{}, fx.Bearer)
		var got map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		return rec.Code, got
	}

	// "me" assigns the caller.
	code, got := put(`{"assignedTo":"me"}`)
	if code != 200 || got["assignee"].(map[string]any)["email"] != fx.UserEmail {
		t.Fatalf("assign me = %d %v", code, got["assignee"])
	}

	// Assign by email.
	code, got = put(`{"assignedTo":"trixie@acme.dev"}`)
	if code != 200 || got["assignee"].(map[string]any)["email"] != "trixie@acme.dev" {
		t.Fatalf("assign email = %d %v", code, got["assignee"])
	}

	// Assign by user id.
	code, got = put(`{"assignedTo":"` + trixieID + `"}`)
	if code != 200 || got["assignee"].(map[string]any)["id"] != trixieID {
		t.Fatalf("assign id = %d %v", code, got["assignee"])
	}

	// Status and assignment combine in one call.
	code, got = put(`{"status":"resolved","assignedTo":"me"}`)
	if code != 200 || got["status"] != "resolved" || got["assignee"].(map[string]any)["email"] != fx.UserEmail {
		t.Fatalf("combined = %d %v %v", code, got["status"], got["assignee"])
	}

	// null unassigns.
	code, got = put(`{"assignedTo":null}`)
	if code != 200 || got["assignee"] != nil {
		t.Fatalf("unassign = %d %v", code, got["assignee"])
	}

	// Non-member → 400.
	if code, _ = put(`{"assignedTo":"nobody@nowhere.io"}`); code != 400 {
		t.Fatalf("non-member = %d (want 400)", code)
	}

	// Nothing to do → 400.
	if code, _ = put(`{}`); code != 400 {
		t.Fatalf("empty update = %d (want 400)", code)
	}
}

func TestIssuesListShowsIngestedIssues(t *testing.T) {
	s := newTestServer(t)
	fx := apiSetup(t, s)

	// Two events of the same error (one affected user) + a different error.
	fx.postErrorEnvelope(t, s, "aa000000000000000000000000000001", "TypeError", "Cannot read properties of undefined", "42", "2026-09-06T10:00:00Z")
	fx.postErrorEnvelope(t, s, "aa000000000000000000000000000002", "TypeError", "Cannot read properties of undefined", "42", "2026-09-06T10:00:01Z")
	fx.postErrorEnvelope(t, s, "aa000000000000000000000000000003", "ReferenceError", "cart is not defined", "43", "2026-09-06T10:00:02Z")

	rec := s.apiCall(t, "GET", fx.issuesPath(), "", jar{}, fx.Bearer)
	if rec.Code != 200 {
		t.Fatalf("issues list = %d: %s", rec.Code, rec.Body.String())
	}
	var got issueListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if got.Meta.Total != 2 || len(got.Data) != 2 {
		t.Fatalf("want 2 issues, got total=%d len=%d", got.Meta.Total, len(got.Data))
	}

	// Newest first: the ReferenceError (last event wins the top slot).
	newest := got.Data[0]
	if newest["title"] != "ReferenceError: cart is not defined" {
		t.Fatalf("first issue = %v", newest["title"])
	}
	if newest["count"] != float64(1) || newest["user_count"] != float64(1) {
		t.Fatalf("newest counters = %v/%v", newest["count"], newest["user_count"])
	}
	if newest["culprit"] != "" { // no frames → title-only grouping
		t.Fatalf("culprit = %v", newest["culprit"])
	}

	// The TypeError issue folded both events, one distinct affected user.
	te := got.Data[1]
	if te["title"] != "TypeError: Cannot read properties of undefined" {
		t.Fatalf("second issue = %v", te["title"])
	}
	if te["count"] != float64(2) || te["user_count"] != float64(1) {
		t.Fatalf("TypeError counters = %v/%v", te["count"], te["user_count"])
	}
	if te["status"] != "unresolved" || te["level"] != "error" || te["type"] != "error" {
		t.Fatalf("TypeError lifecycle fields = %v/%v/%v", te["status"], te["level"], te["type"])
	}
	if te["id"] == "" || te["first_seen"] == "" || te["last_seen"] == "" {
		t.Fatalf("identity/timestamps missing: %v", te)
	}
}

func TestIssuesListEmpty(t *testing.T) {
	s := newTestServer(t)
	fx := apiSetup(t, s)

	// Bearer token auth.
	rec := s.apiCall(t, "GET", fx.issuesPath(), "", jar{}, fx.Bearer)
	if rec.Code != 200 {
		t.Fatalf("issues list = %d: %s", rec.Code, rec.Body.String())
	}
	var got issueListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	// Strict envelope: the fallback project-detail subtree response (which has
	// no "data"/"meta") must not satisfy this.
	if len(got.Data) != 0 || got.Meta.Total != 0 || got.Meta.Limit != 25 || got.Meta.Offset != 0 {
		t.Fatalf("expected empty list with default paging, got %+v", got)
	}

	// Session auth works too.
	sess, _ := login(t, s, fx.UserEmail, "hunter2hunter2")
	if rec := s.get(t, fx.issuesPath(), sess, nil); rec.Code != 200 {
		t.Fatalf("session issues list = %d: %s", rec.Code, rec.Body.String())
	}

	// Unauthenticated → 401.
	if rec := s.get(t, fx.issuesPath(), jar{}, nil); rec.Code != 401 {
		t.Fatalf("unauthenticated = %d (want 401)", rec.Code)
	}
}
