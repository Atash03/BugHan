package hll

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"testing"
)

// Sketches power crash-free user rates: distinct did counts per rollup row
// that merge across deliveries. Tests assert observable behavior: cardinality
// accuracy, dedupe, merge algebra, and serialization — not register layout.

func distinctValues(n int, seed int64) [][]byte {
	r := rand.New(rand.NewSource(seed))
	out := make([][]byte, n)
	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		for {
			var buf [8]byte
			binary.BigEndian.PutUint64(buf[:], r.Uint64())
			if !seen[string(buf[:])] {
				seen[string(buf[:])] = true
				out[i] = buf[:]
				break
			}
		}
	}
	return out
}

func TestEmptySketchEstimatesZero(t *testing.T) {
	if got := New().Estimate(); got != 0 {
		t.Fatalf("empty sketch Estimate() = %d, want 0", got)
	}
}

func TestDuplicatesDoNotInflateEstimate(t *testing.T) {
	s := New()
	v := []byte("user-42")
	for i := 0; i < 1000; i++ {
		s.Insert(v)
	}
	if got := s.Estimate(); got != 1 {
		t.Fatalf("1000 inserts of one value → Estimate() = %d, want 1", got)
	}
}

func TestSmallDistinctCountWithinTolerance(t *testing.T) {
	s := New()
	for _, v := range distinctValues(100, 1) {
		s.Insert(v)
	}
	if got := s.Estimate(); got < 90 || got > 110 {
		t.Fatalf("Estimate() = %d for 100 distinct values, want within [90,110]", got)
	}
}

func TestLargeDistinctCountWithinFivePercent(t *testing.T) {
	s := New()
	for _, v := range distinctValues(10000, 2) {
		s.Insert(v)
	}
	got := s.Estimate()
	if got < 9500 || got > 10500 {
		t.Fatalf("Estimate() = %d for 10000 distinct values, want within [9500,10500]", got)
	}
}

func TestMergeEqualsSingleSketch(t *testing.T) {
	vals := distinctValues(2000, 3)
	whole := New()
	for _, v := range vals {
		whole.Insert(v)
	}
	split := New()
	for _, v := range vals[:800] {
		split.Insert(v)
	}
	other := New()
	for _, v := range vals[800:] {
		other.Insert(v)
	}
	split.Merge(other)
	// Merged sketch and its shards' union must agree closely, and merging is
	// commutative.
	antisym := New()
	antisym.InsertAll(vals[800:])
	antisym.Merge(split)
	if whole.Estimate() != split.Estimate() {
		t.Fatalf("merged Estimate() = %d, single-sketch = %d", split.Estimate(), whole.Estimate())
	}
	if split.Estimate() != antisym.Estimate() {
		t.Fatalf("merge not commutative: %d vs %d", split.Estimate(), antisym.Estimate())
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	s := New()
	for _, v := range distinctValues(500, 4) {
		s.Insert(v)
	}
	blob, err := s.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	back, err := UnmarshalBinary(blob)
	if err != nil {
		t.Fatalf("UnmarshalBinary: %v", err)
	}
	if back.Estimate() != s.Estimate() {
		t.Fatalf("round-trip Estimate() = %d, want %d", back.Estimate(), s.Estimate())
	}
	if !bytes.Equal(blob, func() []byte { b, _ := back.MarshalBinary(); return b }()) {
		t.Fatal("round-trip bytes differ")
	}
}

func TestUnmarshalRejectsBadBlobs(t *testing.T) {
	if _, err := UnmarshalBinary([]byte("too short")); err == nil {
		t.Fatal("UnmarshalBinary accepted a short blob")
	}
}
