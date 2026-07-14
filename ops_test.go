package cowdiff

import (
	"bytes"
	"math/rand"
	"testing"
)

// twoStep builds base -> v1 -> v2, produces the two content diffs, and asserts
// the full operation matrix: each diff round-trips (via applyAll), the chain
// reconstructs v2, and merge(d1,d2) applied to base equals v2. Returns the
// merged diff so callers can make structural assertions on overlaps. All diffs
// use blockSize so segment boundaries land near the byte regions.
func twoStep(t *testing.T, base, v1, v2 []byte, blockSize int) []byte {
	t.Helper()
	d1 := contentDiffBytes(t, base, v1, WithBlockSize(blockSize))
	d2 := contentDiffBytes(t, v1, v2, WithBlockSize(blockSize))
	checkWellFormed(t, d1, uint64(len(v1)))
	checkWellFormed(t, d2, uint64(len(v2)))

	if got := applyAll(t, base, [][]byte{d1}); !bytes.Equal(got, v1) {
		t.Fatal("d1 round-trip != v1")
	}
	if got := applyAll(t, v1, [][]byte{d2}); !bytes.Equal(got, v2) {
		t.Fatal("d2 round-trip != v2")
	}
	if got := applyAll(t, base, [][]byte{d1, d2}); !bytes.Equal(got, v2) {
		t.Fatal("chain apply != v2")
	}
	merged := mergeBytes(t, [][]byte{d1, d2})
	checkWellFormed(t, merged, uint64(len(v2)))
	if got := applyAll(t, base, [][]byte{merged}); !bytes.Equal(got, v2) {
		t.Fatal("merge then apply != v2")
	}
	return merged
}

// Overlap: two DATA writes to an overlapping range; the later one must win.
func TestOverlapDataOverData(t *testing.T) {
	rng := rand.New(rand.NewSource(20))
	base := randBytes(rng, 32*1024)
	blk := 4096
	v1 := append([]byte(nil), base...)
	rng.Read(v1[2*blk : 5*blk]) // blocks 2..4
	v2 := append([]byte(nil), v1...)
	rng.Read(v2[4*blk : 7*blk]) // blocks 4..6, overlaps block 4
	twoStep(t, base, v1, v2, blk)
}

// Overlap: a later ZERO must win over an earlier DATA in the overlap.
func TestOverlapZeroOverData(t *testing.T) {
	rng := rand.New(rand.NewSource(21))
	base := randBytes(rng, 32*1024)
	blk := 4096
	v1 := append([]byte(nil), base...)
	rng.Read(v1[2*blk : 6*blk])
	v2 := append([]byte(nil), v1...)
	for i := 4 * blk; i < 8*blk; i++ {
		v2[i] = 0 // zero blocks 4..7, overlaps the DATA at blocks 4..5
	}
	merged := twoStep(t, base, v1, v2, blk)
	// Structural: in the overlap (block 5) the merged segment must be ZERO.
	h, _ := ReadHeader(bytes.NewReader(merged))
	s := segmentAt(h, uint64(5*blk))
	if s == nil || s.Type != SegZero {
		t.Fatalf("overlap block should be ZERO in merged diff, got %+v", s)
	}
}

// Overlap: a later DATA must win over an earlier ZERO.
func TestOverlapDataOverZero(t *testing.T) {
	rng := rand.New(rand.NewSource(22))
	base := randBytes(rng, 32*1024)
	blk := 4096
	v1 := append([]byte(nil), base...)
	for i := 2 * blk; i < 6*blk; i++ {
		v1[i] = 0
	}
	v2 := append([]byte(nil), v1...)
	rng.Read(v2[4*blk : 8*blk])
	merged := twoStep(t, base, v1, v2, blk)
	h, _ := ReadHeader(bytes.NewReader(merged))
	s := segmentAt(h, uint64(5*blk))
	if s == nil || s.Type != SegData {
		t.Fatalf("overlap block should be DATA in merged diff, got %+v", s)
	}
}

func TestOverlapZeroOverZero(t *testing.T) {
	rng := rand.New(rand.NewSource(23))
	base := randBytes(rng, 32*1024)
	blk := 4096
	v1 := append([]byte(nil), base...)
	for i := 2 * blk; i < 6*blk; i++ {
		v1[i] = 0
	}
	v2 := append([]byte(nil), v1...)
	for i := 4 * blk; i < 8*blk; i++ {
		v2[i] = 0
	}
	twoStep(t, base, v1, v2, blk)
}

// Nested (v2's change fully inside v1's) and exactly-adjacent (coalescing).
func TestOverlapNestedAndTouching(t *testing.T) {
	rng := rand.New(rand.NewSource(24))
	base := randBytes(rng, 64*1024)
	blk := 4096

	// nested
	v1 := append([]byte(nil), base...)
	rng.Read(v1[2*blk : 10*blk])
	v2 := append([]byte(nil), v1...)
	rng.Read(v2[4*blk : 6*blk])
	twoStep(t, base, v1, v2, blk)

	// touching: v1 changes [2,4), v2 changes [4,6) -> adjacent regions
	w1 := append([]byte(nil), base...)
	rng.Read(w1[2*blk : 4*blk])
	w2 := append([]byte(nil), w1...)
	rng.Read(w2[4*blk : 6*blk])
	merged := twoStep(t, base, w1, w2, blk)
	// [2,6) should coalesce into one DATA segment in the merged diff.
	h, _ := ReadHeader(bytes.NewReader(merged))
	if s := segmentAt(h, uint64(3*blk)); s == nil || s.Type != SegData || s.Offset != uint64(2*blk) || s.Offset+s.Length < uint64(6*blk) {
		t.Fatalf("touching regions should coalesce to one DATA [2blk,6blk), got %+v", s)
	}
}

func TestGrowShrink(t *testing.T) {
	rng := rand.New(rand.NewSource(25))
	base := randBytes(rng, 32*1024)
	blk := 4096

	// grow then shrink below the growth
	v1 := append(append([]byte(nil), base...), randBytes(rng, 20*1024)...)
	v2 := append([]byte(nil), v1[:24*1024]...) // shrink below base too
	twoStep(t, base, v1, v2, blk)

	// shrink then regrow (regrow region must be fully specified)
	w1 := append([]byte(nil), base[:8*1024]...)
	w2 := append(append([]byte(nil), w1...), randBytes(rng, 30*1024)...)
	twoStep(t, base, w1, w2, blk)

	// grow with zeros (sparse extension) then write into it
	x1 := append(append([]byte(nil), base...), make([]byte, 40*1024)...)
	x2 := append([]byte(nil), x1...)
	rng.Read(x2[50*1024 : 60*1024])
	twoStep(t, base, x1, x2, blk)
}

func TestEdgeSizes(t *testing.T) {
	rng := rand.New(rand.NewSource(26))
	cases := []struct {
		name       string
		base, newB []byte
	}{
		{"empty-to-data", nil, randBytes(rng, 5000)},
		{"data-to-empty", randBytes(rng, 5000), nil},
		{"both-empty", nil, nil},
		{"one-byte-change", []byte{1}, []byte{2}},
		{"sub-block", randBytes(rng, 100), randBytes(rng, 130)},
		{"grow-all-zero", randBytes(rng, 4096), append(randBytes(rng, 4096), make([]byte, 8192)...)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := contentDiffBytes(t, c.base, c.newB, WithBlockSize(64))
			checkWellFormed(t, d, uint64(len(c.newB)))
			// both-empty and empty-to-empty may have zero segments; apply must
			// still reproduce newB.
			if len(c.newB) == 0 && len(c.base) == 0 {
				// no-op diff on empty base; applyAll needs >=1 diff and works.
			}
			got := applyAll(t, c.base, [][]byte{d})
			if !bytes.Equal(got, c.newB) {
				t.Fatalf("%s: got %d bytes want %d", c.name, len(got), len(c.newB))
			}
		})
	}
}

// No-change diff: identical base and new -> zero segments -> apply == base.
func TestNoChange(t *testing.T) {
	rng := rand.New(rand.NewSource(27))
	base := randBytes(rng, 10*1024)
	d := contentDiffBytes(t, base, base, WithBlockSize(4096))
	h, _ := ReadHeader(bytes.NewReader(d))
	if len(h.Segments) != 0 {
		t.Fatalf("no-change diff should have 0 segments, got %d", len(h.Segments))
	}
	checkWellFormed(t, d, uint64(len(base)))
	if got := applyAll(t, base, [][]byte{d}); !bytes.Equal(got, base) {
		t.Fatal("no-change apply != base")
	}
}

// Pure-resize diffs carry no data and exercise per-diff truncate ordering.
func TestPureResize(t *testing.T) {
	rng := rand.New(rand.NewSource(28))
	base := randBytes(rng, 16*1024)

	// shrink-only: identical prefix -> zero segments, smaller target
	shrunk := append([]byte(nil), base[:6*1024]...)
	d := contentDiffBytes(t, base, shrunk, WithBlockSize(4096))
	h, _ := ReadHeader(bytes.NewReader(d))
	if len(h.Segments) != 0 {
		t.Fatalf("shrink-only should have 0 segments, got %d", len(h.Segments))
	}
	if got := applyAll(t, base, [][]byte{d}); !bytes.Equal(got, shrunk) {
		t.Fatal("shrink-only apply mismatch")
	}

	// grow-all-zero: extension is zeros -> handled by target_size, no segments
	grown := append(append([]byte(nil), base...), make([]byte, 8*1024)...)
	d2 := contentDiffBytes(t, base, grown, WithBlockSize(4096))
	if got := applyAll(t, base, [][]byte{d2}); !bytes.Equal(got, grown) {
		t.Fatal("grow-all-zero apply mismatch")
	}
}
