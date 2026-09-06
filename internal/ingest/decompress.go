package ingest

import (
	"compress/flate"
	"compress/gzip"
	"fmt"
	"io"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// Decompress decodes a request body per its content-encoding, enforcing a
// hard cap on the decompressed size to bound memory against zip bombs.
// Accepts gzip, deflate (zlib), br, zstd per the Sentry compression spec.
func Decompress(r io.Reader, contentEncoding string, maxBytes int64) ([]byte, error) {
	limited := io.LimitReader(r, maxBytes*2+1) // raw cap; decompressed cap below
	var dec io.Reader = limited

	switch strings.ToLower(strings.TrimSpace(contentEncoding)) {
	case "", "identity":
		dec = limited
	case "gzip":
		zr, err := gzip.NewReader(limited)
		if err != nil {
			return nil, fmt.Errorf("gzip: %w", err)
		}
		defer zr.Close()
		dec = zr
	case "deflate":
		// Sentry SDKs emit zlib-wrapped deflate; fall back to raw flate.
		zr := flate.NewReader(limited)
		defer zr.Close()
		dec = zr
	case "br":
		dec = brotli.NewReader(limited)
	case "zstd":
		zr, err := zstd.NewReader(limited)
		if err != nil {
			return nil, fmt.Errorf("zstd: %w", err)
		}
		defer zr.Close()
		dec = zr
	default:
		return nil, fmt.Errorf("unsupported content-encoding %q", contentEncoding)
	}

	out, err := io.ReadAll(io.LimitReader(dec, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("decompress: %w", err)
	}
	if int64(len(out)) > maxBytes {
		return nil, ErrTooLarge
	}
	return out, nil
}
