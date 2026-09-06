package ingest

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
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
		// SDKs emit zlib-wrapped deflate (pako default); raw DEFLATE is the
		// fallback for senders that skip the zlib wrapper. Both decoders must
		// see the same bytes, so the body is buffered once.
		raw, err := io.ReadAll(limited)
		if err != nil {
			return nil, fmt.Errorf("read body: %w", err)
		}
		if len(raw) == 0 {
			return raw, nil
		}
		if zr, zerr := zlib.NewReader(bytes.NewReader(raw)); zerr == nil {
			out, derr := readCapped(zr, maxBytes)
			zr.Close()
			if derr == nil {
				return out, nil
			}
			if errors.Is(derr, ErrTooLarge) {
				return nil, derr
			}
		}
		return readCapped(flate.NewReader(bytes.NewReader(raw)), maxBytes)
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

	return readCapped(dec, maxBytes)
}

func readCapped(dec io.Reader, maxBytes int64) ([]byte, error) {
	out, err := io.ReadAll(io.LimitReader(dec, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("decompress: %w", err)
	}
	if int64(len(out)) > maxBytes {
		return nil, ErrTooLarge
	}
	return out, nil
}
