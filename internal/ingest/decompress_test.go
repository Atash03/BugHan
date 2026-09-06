package ingest

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

const decompressPayload = `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}
{"type":"event"}
{"timestamp":"2026-09-06T10:00:00Z","message":"compressed"}`

func compressWith(t *testing.T, enc string, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	switch enc {
	case "gzip":
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(data)
		_ = zw.Close()
	case "zlib":
		zw := zlib.NewWriter(&buf)
		_, _ = zw.Write(data)
		_ = zw.Close()
	case "deflate-raw":
		zw, _ := flate.NewWriter(&buf, flate.DefaultCompression)
		_, _ = zw.Write(data)
		_ = zw.Close()
	case "br":
		zw := brotli.NewWriter(&buf)
		_, _ = zw.Write(data)
		_ = zw.Close()
	case "zstd":
		zw, _ := zstd.NewWriter(&buf)
		_, _ = zw.Write(data)
		_ = zw.Close()
	}
	return buf.Bytes()
}

func TestDecompressAllEncodings(t *testing.T) {
	// "deflate" must accept both the zlib-wrapped form real SDKs emit and
	// bare raw flate from non-conforming senders.
	cases := []struct {
		encoding string
		body     []byte
	}{
		{"", []byte(decompressPayload)},
		{"identity", []byte(decompressPayload)},
		{"gzip", compressWith(t, "gzip", []byte(decompressPayload))},
		{"deflate", compressWith(t, "zlib", []byte(decompressPayload))},
		{"deflate", compressWith(t, "deflate-raw", []byte(decompressPayload))},
		{"DEFLATE", compressWith(t, "zlib", []byte(decompressPayload))}, // case-insensitive
		{"br", compressWith(t, "br", []byte(decompressPayload))},
		{"zstd", compressWith(t, "zstd", []byte(decompressPayload))},
	}
	for _, tc := range cases {
		got, err := Decompress(bytes.NewReader(tc.body), tc.encoding, 1<<20)
		if err != nil {
			t.Errorf("Decompress(%q) error: %v", tc.encoding, err)
			continue
		}
		if string(got) != decompressPayload {
			t.Errorf("Decompress(%q) = %q, want payload intact", tc.encoding, got)
		}
	}
}

func TestDecompressEmptyBody(t *testing.T) {
	// Empty bodies decode to empty for identity and deflate — the envelope
	// parser (not decompression) owns rejecting them.
	for _, enc := range []string{"", "deflate"} {
		if _, err := Decompress(bytes.NewReader(nil), enc, 1<<20); err != nil {
			t.Errorf("Decompress(%q) empty body error: %v", enc, err)
		}
	}
}

func TestDecompressRejects(t *testing.T) {
	cases := []struct {
		label    string
		encoding string
		body     []byte
	}{
		{"unsupported encoding", "brotli-dictionary", []byte("x")},
		{"corrupt gzip", "gzip", []byte("not gzip")},
		{"garbage deflate", "deflate", []byte{0xff, 0xff, 0xff, 0xff}},
	}
	for _, tc := range cases {
		if _, err := Decompress(bytes.NewReader(tc.body), tc.encoding, 1<<20); err == nil {
			t.Errorf("%s: expected error, got none", tc.label)
		}
	}
}

func TestDecompressTooLarge(t *testing.T) {
	big := strings.Repeat("A", 4096)

	// Decompressed size over the cap → ErrTooLarge (413 upstream), gzip.
	if _, err := Decompress(bytes.NewReader(compressWith(t, "gzip", []byte(big))), "gzip", 1024); !errors.Is(err, ErrTooLarge) {
		t.Errorf("gzip over cap: err = %v, want ErrTooLarge", err)
	}
	// zlib-wrapped deflate must hit the same cap without falling back to the
	// raw-flate decoder.
	if _, err := Decompress(bytes.NewReader(compressWith(t, "zlib", []byte(big))), "deflate", 1024); !errors.Is(err, ErrTooLarge) {
		t.Errorf("zlib deflate over cap: err = %v, want ErrTooLarge", err)
	}
	// Uncompressed body over the cap.
	if _, err := Decompress(bytes.NewReader([]byte(big)), "", 1024); !errors.Is(err, ErrTooLarge) {
		t.Errorf("identity over cap: err = %v, want ErrTooLarge", err)
	}
}
