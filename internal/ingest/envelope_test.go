package ingest

import (
	"strings"
	"testing"
)

// Realistic browser-SDK envelope from the research fixtures.
const errorEnvelope = `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc","sent_at":"2026-09-06T10:00:00.000Z","sdk":{"name":"sentry.javascript.browser","version":"10.70.0"}}
{"type":"event"}
{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc","timestamp":"2026-09-06T10:00:00.000Z","platform":"javascript","level":"error","environment":"production","release":"app@1.0.0","tags":{"handled":"no"},"user":{"id":"42","email":"u@example.com"},"exception":{"values":[{"type":"TypeError","value":"Cannot read properties of undefined (reading 'foo')","stacktrace":{"frames":[{"filename":"https://example.com/static/js/main.abc123.js","function":"App.render","lineno":42,"colno":7,"in_app":true}]}}]},"breadcrumbs":{"values":[{"type":"navigation","category":"navigation","timestamp":"2026-09-06T09:59:00.000Z","data":{"from":"/login","to":"/dashboard"}}]}}`

const transactionEnvelope = `{"event_id":"743ad8bbfdd84e99bc38b4729e2864de","sent_at":"2026-09-06T10:00:01.000Z","sdk":{"name":"sentry.javascript.browser","version":"10.70.0"},"trace":{"trace_id":"743ad8bbfdd84e99bc38b4729e2864de","public_key":"k","sample_rate":"1","transaction":"/users/:id"}}
{"type":"transaction","event_id":"743ad8bbfdd84e99bc38b4729e2864de"}
{"type":"transaction","event_id":"743ad8bbfdd84e99bc38b4729e2864de","transaction":"/users/:id","transaction_info":{"source":"route"},"start_timestamp":1786000000.0,"timestamp":1786000001.234,"contexts":{"trace":{"trace_id":"743ad8bbfdd84e99bc38b4729e2864de","span_id":"a0cfbde2bdff3adc","op":"pageload","status":"ok"}},"spans":[{"span_id":"b01b9f6349558cd1","parent_span_id":"a0cfbde2bdff3adc","trace_id":"743ad8bbfdd84e99bc38b4729e2864de","op":"http.client","description":"GET https://api.example.com/users","start_timestamp":1786000000.100,"timestamp":1786000000.350,"status":"ok"}],"measurements":{"lcp":{"value":2049,"unit":"millisecond"},"cls":{"value":0.21,"unit":""}}}`

const sessionEnvelope = `{"sent_at":"2026-09-06T10:00:02.000Z","sdk":{"name":"sentry.javascript.browser","version":"10.70.0"}}
{"type":"session"}
{"sid":"7c7b6585-f901-4351-bf8d-02711b721929","init":true,"started":"2026-09-06T10:00:00.000Z","timestamp":"2026-09-06T10:02:10.000Z","status":"crashed","errors":1,"duration":130,"attrs":{"release":"app@1.0.0","environment":"production"}}`

const feedbackEnvelope = `{"event_id":"d5f8d0e2a2c14d8e9f1a2b3c4d5e6f70","sent_at":"2026-09-06T10:00:03.000Z"}
{"type":"feedback"}
{"event_id":"d5f8d0e2a2c14d8e9f1a2b3c4d5e6f70","timestamp":"2026-09-06T10:00:03.000Z","platform":"javascript","contexts":{"feedback":{"message":"The save button did nothing","contact_email":"end@user.io","name":"End User","associated_event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}}}`

const sessionAggregatesEnvelope = `{"sent_at":"2026-09-06T11:00:00.000Z"}
{"type":"sessions"}
{"aggregates":[{"started":"2026-09-06T10:30:00.000Z","exited":40,"crashed":2,"abnormal":1,"errored":3}],"attrs":{"release":"app@1.0.0","environment":"production"}}`

const clientReportEnvelope = `{"sent_at":"2026-09-06T10:00:04.000Z"}
{"type":"client_report"}
{"timestamp":"2026-09-06T10:00:04.000Z","discarded_events":[{"reason":"queue_overflow","category":"error","quantity":23}]}`

func TestParseEnvelopeErrorItem(t *testing.T) {
	env, err := ParseEnvelope([]byte(errorEnvelope))
	if err != nil {
		t.Fatal(err)
	}
	if env.Header.EventID != "9ec79c33ec9942ab8353589fcb2e04dc" {
		t.Fatalf("header event_id = %q", env.Header.EventID)
	}
	if len(env.Items) != 1 || env.Items[0].Header.Type != "event" {
		t.Fatalf("items = %+v", env.Items)
	}
	if !strings.Contains(string(env.Items[0].Payload), "TypeError") {
		t.Fatal("payload lost")
	}
}

func TestParseEnvelopeLengthPrefixed(t *testing.T) {
	// Attachments with binary payloads containing newlines require `length`.
	body := "{\"event_id\":\"a\"}\n" +
		"{\"type\":\"attachment\",\"length\":8,\"content_type\":\"text/plain\",\"filename\":\"hello.txt\"}\n" +
		"Hello\r\n\n\n" +
		"{\"type\":\"event\"}\n{}\n"
	env, err := ParseEnvelope([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(env.Items) != 2 {
		t.Fatalf("items = %d", len(env.Items))
	}
	if string(env.Items[0].Payload) != "Hello\r\n\n" {
		t.Fatalf("length-prefixed payload = %q", env.Items[0].Payload)
	}
	if env.Items[1].Header.Type != "event" {
		t.Fatalf("second item = %q", env.Items[1].Header.Type)
	}
}

func TestParseEnvelopeUnknownHeadersRetained(t *testing.T) {
	body := "{\"event_id\":\"a\",\"future_header\":{\"x\":1}}\n{\"type\":\"event\",\"new_field\":\"v\"}\n{}\n"
	env, err := ParseEnvelope([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if env.Items[0].Header.Type != "event" {
		t.Fatal("type lost")
	}
	if env.Header.raw == nil || len(env.Header.raw["future_header"]) == 0 {
		t.Fatal("unknown envelope header dropped (spec: retained)")
	}
}

func TestParseEnvelopeDSNPublicKey(t *testing.T) {
	h := Header{DSN: "https://e12d836b15bb49d7bbf99e64295d995b@bugs.example.com/42"}
	if got := h.DSNPublicKey(); got != "e12d836b15bb49d7bbf99e64295d995b" {
		t.Fatalf("key = %q", got)
	}
	h = Header{DSN: "https://key:secret@sentry.io/42"}
	if got := h.DSNPublicKey(); got != "key" {
		t.Fatalf("key with secret = %q", got)
	}
}

func TestParseXSentryAuthHeader(t *testing.T) {
	key, ver := ParseXSentryAuth("Sentry sentry_version=7, sentry_client=sentry.javascript.browser/10.70.0, sentry_key=abc123")
	if key != "abc123" || ver != "7" {
		t.Fatalf("key=%q version=%q", key, ver)
	}
}

func TestParseEnvelopeRejectsGarbage(t *testing.T) {
	if _, err := ParseEnvelope([]byte("")); err == nil {
		t.Fatal("empty accepted")
	}
	if _, err := ParseEnvelope([]byte("not json\n")); err == nil {
		t.Fatal("bad header accepted")
	}
	// Item declaring more bytes than available.
	if _, err := ParseEnvelope([]byte("{}\n{\"type\":\"event\",\"length\":100}\nshort")); err == nil {
		t.Fatal("overlong item accepted")
	}
}
