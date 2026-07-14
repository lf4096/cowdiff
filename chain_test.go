package cowdiff

import (
	"bytes"
	"math/rand"
	"testing"
)

// buildChain applies K random mutations to a model, returning the base bytes,
// the per-version bytes, and the per-step content diffs.
func buildChain(t *testing.T, rng *rand.Rand, initial []byte, K, blockSize int) (base []byte, versions [][]byte, diffs [][]byte) {
	t.Helper()
	m := &model{b: append([]byte(nil), initial...)}
	base = m.bytes()
	prev := base
	var st opStats
	st.ops = map[string]bool{}
	for i := 0; i < K; i++ {
		mutate(rng, m, i, &st)
		scatter(rng, m, 1+rng.Intn(4)) // multi-segment diffs
		cur := m.bytes()
		d := contentDiffBytes(t, prev, cur, WithBlockSize(blockSize))
		checkWellFormed(t, d, uint64(len(cur)))
		if got := applyAll(t, prev, [][]byte{d}); !bytes.Equal(got, cur) {
			t.Fatalf("step %d round-trip mismatch", i)
		}
		versions = append(versions, cur)
		diffs = append(diffs, d)
		prev = cur
	}
	return base, versions, diffs
}

func TestLongChain(t *testing.T) {
	rng := rand.New(rand.NewSource(30))
	base, versions, diffs := buildChain(t, rng, randBytes(rng, 200*1024), 16, 4096)
	K := len(diffs)

	// I5: restore at every point p.
	for p := 1; p <= K; p++ {
		if got := applyAll(t, base, diffs[:p]); !bytes.Equal(got, versions[p-1]) {
			t.Fatalf("restore p=%d mismatch", p)
		}
	}

	// I2: merge the full chain.
	merged := mergeBytes(t, diffs)
	checkWellFormed(t, merged, uint64(len(versions[K-1])))
	if got := applyAll(t, base, [][]byte{merged}); !bytes.Equal(got, versions[K-1]) {
		t.Fatal("full merge apply mismatch")
	}
}

func TestPartialMergeSplice(t *testing.T) {
	rng := rand.New(rand.NewSource(31))
	base, versions, diffs := buildChain(t, rng, randBytes(rng, 150*1024), 12, 4096)
	K := len(diffs)
	final := versions[K-1]

	// I6: merge a contiguous middle sub-chain [i,j] and splice it back.
	for _, ij := range [][2]int{{1, 4}, {3, 8}, {0, 5}, {6, 11}} {
		i, j := ij[0], ij[1]
		sub := mergeBytes(t, diffs[i:j+1])
		checkWellFormed(t, sub, uint64(len(versions[j])))
		spliced := make([][]byte, 0, K)
		spliced = append(spliced, diffs[:i]...)
		spliced = append(spliced, sub)
		spliced = append(spliced, diffs[j+1:]...)
		if got := applyAll(t, base, spliced); !bytes.Equal(got, final) {
			t.Fatalf("sub-merge splice [%d,%d] mismatch", i, j)
		}
	}
}

func TestRollForward(t *testing.T) {
	rng := rand.New(rand.NewSource(32))
	base, versions, diffs := buildChain(t, rng, randBytes(rng, 180*1024), 14, 4096)
	K := len(diffs)
	final := versions[K-1]

	// I4: roll the base forward at several cut points.
	for _, cut := range []int{1, 5, 9, 13} {
		rolledBase := applyAll(t, base, diffs[:cut]) // == versions[cut-1]
		if !bytes.Equal(rolledBase, versions[cut-1]) {
			t.Fatalf("rolled base at cut=%d != versions[%d]", cut, cut-1)
		}
		if got := applyAll(t, rolledBase, diffs[cut:]); !bytes.Equal(got, final) {
			t.Fatalf("roll-forward cut=%d mismatch", cut)
		}
	}
}

func TestDisjointMergeSplice(t *testing.T) {
	rng := rand.New(rand.NewSource(33))
	base, versions, diffs := buildChain(t, rng, randBytes(rng, 120*1024), 12, 4096)
	final := versions[len(versions)-1]

	// Merge several disjoint contiguous chunks, splice all back, apply == full.
	chunks := [][2]int{{0, 2}, {3, 3}, {4, 7}, {8, 11}}
	var spliced [][]byte
	for _, c := range chunks {
		spliced = append(spliced, mergeBytes(t, diffs[c[0]:c[1]+1]))
	}
	if got := applyAll(t, base, spliced); !bytes.Equal(got, final) {
		t.Fatal("disjoint merge splice mismatch")
	}
}

func TestSingleDiffMerge(t *testing.T) {
	rng := rand.New(rand.NewSource(34))
	base := randBytes(rng, 40*1024)
	v1 := append([]byte(nil), base...)
	rng.Read(v1[3*1024 : 9*1024])

	d1 := contentDiffBytes(t, base, v1, WithBlockSize(4096))
	// Merge of a single diff must reproduce that diff's effect.
	m1 := mergeBytes(t, [][]byte{d1})
	checkWellFormed(t, m1, uint64(len(v1)))
	if got := applyAll(t, base, [][]byte{m1}); !bytes.Equal(got, v1) {
		t.Fatal("single-diff merge apply mismatch")
	}
}

// from_hash carry-through: merged carries the FIRST diff's from_hash, and a
// later diff's from_hash is dropped.
func TestMergeFromHash(t *testing.T) {
	rng := rand.New(rand.NewSource(35))
	base := randBytes(rng, 32*1024)
	v1 := append([]byte(nil), base...)
	rng.Read(v1[1024:4096])
	v2 := append([]byte(nil), v1...)
	rng.Read(v2[8*1024 : 12*1024])

	h1 := "1111111111111111111111111111111111111111111111111111111111111111"
	h2 := "2222222222222222222222222222222222222222222222222222222222222222"
	d1 := contentDiffBytes(t, base, v1, WithFromHash(h1), WithBlockSize(4096))
	d2 := contentDiffBytes(t, v1, v2, WithFromHash(h2), WithBlockSize(4096))

	merged := mergeBytes(t, [][]byte{d1, d2})
	h, _ := ReadHeader(bytes.NewReader(merged))
	if h.FromHash != h1 {
		t.Fatalf("merged from_hash = %q, want first diff's %q", h.FromHash, h1)
	}
	if got := applyAll(t, base, [][]byte{merged}); !bytes.Equal(got, v2) {
		t.Fatal("merge apply mismatch")
	}
}

// TestMergeGrowThenShrink exercises resolveChain's segment clamping when an
// intermediate diff grows the file and a later one shrinks below the growth.
func TestMergeGrowThenShrink(t *testing.T) {
	rng := rand.New(rand.NewSource(36))
	base := randBytes(rng, 20*1024)
	v1 := append(append([]byte(nil), base...), randBytes(rng, 40*1024)...) // grow to 60k
	v2 := append([]byte(nil), v1[:30*1024]...)                             // shrink to 30k
	rng.Read(v2[25*1024 : 30*1024])                                        // change near the new tail

	merged := twoStep(t, base, v1, v2, 4096) // asserts chain == merge == v2
	checkWellFormed(t, merged, uint64(len(v2)))
}
