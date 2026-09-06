// Package ingest implements Sentry envelope parsing and the ingest pipeline.
package ingest

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Envelope is a parsed Sentry envelope: one header + items.
type Envelope struct {
	Header Header
	Items  []Item
}

// Header is the envelope header (event_id, dsn, sdk, sent_at, trace…).
type Header struct {
	EventID string          `json:"event_id"`
	DSN     string          `json:"dsn"`
	SentAt  string          `json:"sent_at"`
	Trace   json.RawMessage `json:"trace"`
	SDK     json.RawMessage `json:"sdk"`
	raw     map[string]json.RawMessage
}

// DSNPublicKey extracts the public key from an envelope `dsn` header value
// (format scheme://key[:secret]@host/projectID).
func (h *Header) DSNPublicKey() string {
	if h.DSN == "" {
		return ""
	}
	schemeEnd := bytesIndex(h.DSN, "://")
	rest := h.DSN
	if schemeEnd >= 0 {
		rest = h.DSN[schemeEnd+3:]
	}
	at := bytesIndex(rest, "@")
	if at < 0 {
		return ""
	}
	key := rest[:at]
	// strip deprecated secret part
	if colon := bytesIndex(key, ":"); colon >= 0 {
		key = key[:colon]
	}
	return key
}

// Item is one envelope item: header + raw payload.
type Item struct {
	Header  ItemHeader
	Payload []byte
}

// ItemHeader is the per-item JSON header line.
type ItemHeader struct {
	Type        string `json:"type"`
	Length      int64  `json:"length,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Filename    string `json:"filename,omitempty"`
}

// MaxEnvelopeItems bounds parser work per envelope.
const MaxEnvelopeItems = 1000

// ParseEnvelope frames an envelope per the Sentry spec: a JSON header line,
// then items of (JSON header line + payload). Payloads are length-prefixed
// when the item header carries `length`, otherwise newline-terminated.
func ParseEnvelope(data []byte) (*Envelope, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty envelope")
	}
	env := &Envelope{}

	// Envelope header: first line (may be empty = {}).
	headerLine, rest, err := nextLine(data)
	if err != nil {
		return nil, fmt.Errorf("envelope header: %w", err)
	}
	if len(headerLine) > 0 {
		if err := json.Unmarshal(headerLine, &env.Header); err != nil {
			return nil, fmt.Errorf("envelope header: %w", err)
		}
	}
	// Keep unknown header attributes (retained per spec, unused for now).
	var attrs map[string]json.RawMessage
	_ = json.Unmarshal(headerLine, &attrs)
	env.Header.raw = attrs

	for len(rest) > 0 && len(env.Items) < MaxEnvelopeItems {
		var itemHeaderLine []byte
		itemHeaderLine, rest, err = nextLine(rest)
		if err != nil {
			return nil, fmt.Errorf("item header: %w", err)
		}
		if len(itemHeaderLine) == 0 {
			continue
		}
		var ih ItemHeader
		if err := json.Unmarshal(itemHeaderLine, &ih); err != nil {
			return nil, fmt.Errorf("item header %q: %w", truncate(itemHeaderLine, 80), err)
		}
		if ih.Type == "" {
			return nil, fmt.Errorf("item header missing type")
		}

		var payload []byte
		if ih.Length > 0 {
			if ih.Length > int64(len(rest)) {
				return nil, fmt.Errorf("item %q declares %d bytes but %d remain", ih.Type, ih.Length, len(rest))
			}
			payload = rest[:ih.Length]
			rest = rest[ih.Length:]
			// A trailing newline after the payload is skipped if present.
			if len(rest) > 0 && rest[0] == '\n' {
				rest = rest[1:]
			}
		} else {
			payload, rest, err = nextLine(rest)
			if err != nil {
				return nil, fmt.Errorf("item %q payload: %w", ih.Type, err)
			}
		}
		env.Items = append(env.Items, Item{Header: ih, Payload: payload})
	}
	return env, nil
}

// nextLine splits data at the first \n; returns the line (without it) and the
// remainder. An envelope ending without a trailing newline still yields the
// final line.
func nextLine(data []byte) (line, rest []byte, err error) {
	i := bytes.IndexByte(data, '\n')
	if i < 0 {
		return data, nil, nil
	}
	return data[:i], data[i+1:], nil
}

func bytesIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
