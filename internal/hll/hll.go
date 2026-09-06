// Package hll implements a minimal HyperLogLog sketch (DESIGN.md §7: HLL
// distinct-did on session rollups). Sketches merge by taking register-wise
// maxima, so crash-free user rates can union across rollup rows, and the
// representation is a fixed bytea blob persisted next to the rollup counts.
package hll

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
)

const (
	// p index bits → 4096 registers ≈ 1.6% standard error; six bits per
	// register caps at 63 leading zeros, far beyond the 64-bit hash domain.
	p        = 12
	m        = 1 << p
	blobSize = m * 6 / 8 // 3072 bytes
)

// Sketch is a HyperLogLog cardinality estimator.
type Sketch struct {
	registers [m]byte
}

// New returns an empty sketch.
func New() *Sketch { return &Sketch{} }

// Insert hashes one value into the sketch. Duplicate values are no-ops.
func (s *Sketch) Insert(v []byte) {
	sum := sha256.Sum256(v)
	s.InsertHash(binary.BigEndian.Uint64(sum[:8]))
}

// InsertHash folds an already-hashed value; exposed so callers hashing in
// bulk don't repeat work.
func (s *Sketch) InsertHash(h uint64) {
	idx := h >> (64 - p)
	remainder := h << p // low 52 bits carry the run of leading zeros
	rank := byte(1)
	for rank < 63 && remainder>>63 == 0 {
		remainder <<= 1
		rank++
	}
	if rank > s.registers[idx] {
		s.registers[idx] = rank
	}
}

// InsertAll convenience for seeding many values.
func (s *Sketch) InsertAll(vs [][]byte) {
	for _, v := range vs {
		s.Insert(v)
	}
}

// Merge unions other into s (register-wise max).
func (s *Sketch) Merge(other *Sketch) {
	for i, r := range other.registers {
		if r > s.registers[i] {
			s.registers[i] = r
		}
	}
}

// Estimate returns the cardinality estimate. Uses linear counting in the
// small-cardinality regime (where the raw estimator is biased), the standard
// harmonic-mean estimator otherwise.
func (s *Sketch) Estimate() uint64 {
	var sum float64
	zeros := 0
	for _, r := range s.registers {
		sum += math.Pow(2, -float64(r))
		if r == 0 {
			zeros++
		}
	}
	raw := alpha() * float64(m) * float64(m) / sum
	if raw <= 2.5*float64(m) && zeros > 0 {
		return uint64(float64(m) * math.Log(float64(m)/float64(zeros)))
	}
	return uint64(raw + 0.5)
}

func alpha() float64 {
	return 0.7213 / (1 + 1.079/float64(m))
}

// MarshalBinary packs the registers six bits each into a fixed-size blob.
func (s *Sketch) MarshalBinary() ([]byte, error) {
	blob := make([]byte, blobSize)
	bit := 0
	for _, r := range s.registers {
		for b := 0; b < 6; b++ {
			if r&(1<<b) != 0 {
				blob[bit/8] |= 1 << (bit % 8)
			}
			bit++
		}
	}
	return blob, nil
}

// UnmarshalBinary restores a sketch from MarshalBinary output.
func UnmarshalBinary(blob []byte) (*Sketch, error) {
	if len(blob) != blobSize {
		return nil, errors.New("hll: blob must be 3072 bytes")
	}
	s := &Sketch{}
	bit := 0
	for i := range s.registers {
		var r byte
		for b := 0; b < 6; b++ {
			if blob[bit/8]&(1<<(bit%8)) != 0 {
				r |= 1 << b
			}
			bit++
		}
		s.registers[i] = r
	}
	return s, nil
}
