package httpapi

import (
	"strings"
	"testing"
)

// T11: feedback attaches to its associated error's issue within a 30-minute
// window; outside the window it stays unlinked.
func TestFeedbackAssociationWindow(t *testing.T) {
	s := newTestServer(t)
	projectID, key := ingestSetup(t, s)

	// Error then feedback 3s later → linked.
	if rec := s.postEnvelope(t, projectID, key, errorEnvelopeTest, ""); rec.Code != 200 {
		t.Fatalf("error = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := s.postEnvelope(t, projectID, key, feedbackEnvelopeTest, ""); rec.Code != 200 {
		t.Fatalf("feedback = %d: %s", rec.Code, rec.Body.String())
	}
	var linked int
	if err := s.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM feedbacks WHERE project_id = $1 AND issue_id IS NOT NULL`, projectID).Scan(&linked); err != nil || linked != 1 {
		t.Fatalf("feedback linked = %d err=%v", linked, err)
	}
	var issueCount int
	if err := s.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM feedbacks f JOIN issues i ON i.id = f.issue_id WHERE f.project_id = $1`, projectID).Scan(&issueCount); err != nil || issueCount != 1 {
		t.Fatalf("feedback→issue join = %d err=%v", issueCount, err)
	}

	// Feedback 2h after its event → stays unlinked.
	late := strings.Replace(feedbackEnvelopeTest,
		`"event_id":"d5f8d0e2a2c14d8e9f1a2b3c4d5e6f70"`,
		`"event_id":"e5f8d0e2a2c14d8e9f1a2b3c4d5e6f71"`, 2)
	late = strings.Replace(late, `2026-09-06T10:00:03.000Z`, `2026-09-06T12:05:00.000Z`, 2)
	if rec := s.postEnvelope(t, projectID, key, late, ""); rec.Code != 200 {
		t.Fatalf("late feedback = %d: %s", rec.Code, rec.Body.String())
	}
	var unlinked int
	if err := s.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM feedbacks WHERE project_id = $1 AND issue_id IS NULL`, projectID).Scan(&unlinked); err != nil || unlinked != 1 {
		t.Fatalf("late feedback should stay unlinked, nulls = %d err=%v", unlinked, err)
	}
}

// T11: feedback arriving before its error backfills when the error lands.
func TestFeedbackBackfillOnLateEvent(t *testing.T) {
	s := newTestServer(t)
	projectID, key := ingestSetup(t, s)

	fb := strings.Replace(feedbackEnvelopeTest,
		`"associated_event_id":"9ec79c33ec9942ab8353589fcb2e04dc"`,
		`"associated_event_id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"`, 1)
	fb = strings.Replace(fb, `"event_id":"d5f8d0e2a2c14d8e9f1a2b3c4d5e6f70"`,
		`"event_id":"ffffffff-1111-2222-3333-444444444444"`, 2)
	fb = strings.Replace(fb, `2026-09-06T10:00:03.000Z`, `2026-09-06T10:00:03.000Z`, 2)
	if rec := s.postEnvelope(t, projectID, key, fb, ""); rec.Code != 200 {
		t.Fatalf("early feedback = %d: %s", rec.Code, rec.Body.String())
	}
	var nulls int
	_ = s.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM feedbacks WHERE project_id = $1 AND issue_id IS NULL`, projectID).Scan(&nulls)
	if nulls != 1 {
		t.Fatalf("early feedback should start unlinked, nulls=%d", nulls)
	}

	ev := strings.Replace(errorEnvelopeTest,
		`9ec79c33ec9942ab8353589fcb2e04dc`, `aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee`, 2)
	if rec := s.postEnvelope(t, projectID, key, ev, ""); rec.Code != 200 {
		t.Fatalf("late error = %d: %s", rec.Code, rec.Body.String())
	}
	var linked int
	if err := s.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM feedbacks WHERE project_id = $1 AND issue_id IS NOT NULL`, projectID).Scan(&linked); err != nil || linked != 1 {
		t.Fatalf("backfill failed, linked=%d err=%v", linked, err)
	}
}
