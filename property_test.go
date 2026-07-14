package cowdiff

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"testing"
)

type opStats struct{ ops map[string]bool }

func (s *opStats) rec(op string) {
	if s.ops == nil {
		s.ops = map[string]bool{}
	}
	s.ops[op] = true
}

func ensureSizeRand(rng *rand.Rand, m *model, n int) {
	if m.size() < n {
		old := m.size()
		m.grow(n)
		rng.Read(m.b[old:])
	}
}

func writeRand(rng *rand.Rand, m *model) {
	if m.size() == 0 {
		m.grow(1 + rng.Intn(8192))
		rng.Read(m.b)
		return
	}
	off := rng.Intn(m.size())
	n := 1 + rng.Intn(m.size()-off)
	m.writeAt(off, randBytes(rng, n))
}

// mutate applies one mutation to m. The first rounds are deterministic to
// guarantee coverage of every op kind plus block-aligned and EOF-exact edits;
// later rounds are random.
func mutate(rng *rand.Rand, m *model, round int, st *opStats) {
	switch round {
	case 0:
		st.rec("writeRand")
		writeRand(rng, m)
	case 1: // block-aligned zero over previously-random blocks -> ZERO segment
		st.rec("zeroAligned")
		ensureSizeRand(rng, m, 32*1024)
		m.zeroAt(2*4096, 2*4096)
	case 2:
		st.rec("shrink")
		if m.size() > 0 {
			m.truncate(m.size() / 2)
		}
	case 3:
		st.rec("extendZero")
		m.truncate(m.size() + 1 + rng.Intn(20000))
	case 4:
		st.rec("extendRand")
		old := m.size()
		m.truncate(old + 1 + rng.Intn(20000))
		m.writeAt(old, randBytes(rng, m.size()-old))
	case 5: // write exactly the last block (EOF-terminating DATA)
		st.rec("eofWrite")
		ensureSizeRand(rng, m, 16*1024)
		off := m.size() - 4096
		m.writeAt(off, randBytes(rng, m.size()-off))
	case 6: // zero the last block (EOF-terminating ZERO)
		st.rec("zeroTail")
		ensureSizeRand(rng, m, 16*1024)
		off := m.size() - 4096
		m.zeroAt(off, m.size()-off)
	case 7:
		st.rec("noop")
	default:
		size := m.size()
		switch rng.Intn(6) {
		case 0:
			st.rec("writeRand")
			writeRand(rng, m)
		case 1:
			st.rec("writeZero")
			if size > 0 {
				off := rng.Intn(size)
				m.zeroAt(off, 1+rng.Intn(size-off))
			}
		case 2:
			st.rec("shrink")
			if size > 0 {
				m.truncate(rng.Intn(size))
			}
		case 3:
			st.rec("extendZero")
			m.truncate(size + 1 + rng.Intn(30000))
		case 4:
			st.rec("extendRand")
			old := size
			m.truncate(old + 1 + rng.Intn(30000))
			m.writeAt(old, randBytes(rng, m.size()-old))
		case 5:
			st.rec("noop")
		}
	}
}

// scatter applies n writes/zeros, one per equal-width region of the file, so a
// single per-step diff carries multiple non-adjacent segments (distinct blocks)
// rather than one contiguous change -- even at large block sizes.
func scatter(rng *rand.Rand, m *model, n int) {
	size := m.size()
	if size == 0 || n <= 0 {
		return
	}
	region := size / n
	if region == 0 {
		region = size
	}
	for k := 0; k < n; k++ {
		lo := k * region
		if lo >= size {
			break
		}
		hi := min(lo+region, size)
		off := lo + rng.Intn(hi-lo)
		length := 1 + rng.Intn(min(hi-off, 8192))
		if rng.Intn(2) == 0 {
			m.writeAt(off, randBytes(rng, length))
		} else {
			m.zeroAt(off, length)
		}
	}
}

func TestPropertyRandom(t *testing.T) {
	// Fixed, reproducible chains spanning the interesting block sizes.
	cfgs := []struct {
		seed int64
		blk  int
		sz   int
	}{
		{1, 1, 16 * 1024},
		{2, 64, 120 * 1024},
		{3, 4096, 200 * 1024},
		{7, 65536, 300 * 1024},
		{42, 512, 90 * 1024},
	}
	for _, c := range cfgs {
		c := c
		t.Run(fmt.Sprintf("seed%d_blk%d", c.seed, c.blk), func(t *testing.T) {
			runPropertyChain(t, c.seed, c.blk, c.sz, 20, true)
		})
	}

	// A fixed count of distinct random chains for breadth. Random testing finds
	// problems by covering many cases, so this always runs; fixed seeds keep any
	// failure reproducible. COWDIFF_FUZZ_CHAINS overrides the count.
	blks := []int{1, 64, 512, 4096, 65536}
	for i := 0; i < envInt("COWDIFF_FUZZ_CHAINS", 100); i++ {
		seed := int64(1000 + i)
		pick := rand.New(rand.NewSource(seed))
		blk := blks[pick.Intn(len(blks))]
		sz := 4*1024 + pick.Intn(128*1024)
		if blk <= 64 {
			sz = 4*1024 + pick.Intn(20*1024) // keep byte/tiny-block cases fast
		}
		k := 3 + pick.Intn(15)
		t.Run(fmt.Sprintf("fuzz%d", i), func(t *testing.T) {
			t.Parallel()
			runPropertyChain(t, seed, blk, sz, k, false)
		})
	}
}

// runPropertyChain builds a K-diff chain, applying a mutation plus several
// scattered edits each round so diffs carry multiple non-adjacent segments, and
// checks every invariant: per-step round-trip via all three apply paths
// (I1/I3), restore at any point (I5), full and partial merge equivalence
// (I2/I6), and roll-forward (I4). With strict it also asserts op- and
// segment-type coverage.
func runPropertyChain(t *testing.T, seed int64, blk, sz, K int, strict bool) {
	t.Helper()
	// strict chains use the full file-based apply (all three paths, fsync); the
	// breadth fuzz uses a fast fsync-free in-memory resolveChain check.
	apply := applyAll
	if !strict {
		apply = reconstructMem
	}
	rng := rand.New(rand.NewSource(seed))
	m := &model{b: randBytes(rng, sz)}
	base := m.bytes()
	prev := base
	var st opStats
	var sawData, sawZero bool
	var maxSegs int
	var versions, diffs [][]byte

	for i := 0; i < K; i++ {
		mutate(rng, m, i, &st)
		scatter(rng, m, 1+rng.Intn(4))
		cur := m.bytes()
		d := contentDiffBytes(t, prev, cur, WithBlockSize(blk))
		checkWellFormed(t, d, uint64(len(cur)))
		h, _ := ReadHeader(bytes.NewReader(d))
		if len(h.Segments) > maxSegs {
			maxSegs = len(h.Segments)
		}
		for _, s := range h.Segments {
			if s.Type == SegData {
				sawData = true
			} else {
				sawZero = true
			}
		}
		if got := apply(t, prev, [][]byte{d}); !bytes.Equal(got, cur) {
			t.Fatalf("step %d round-trip mismatch at byte %d", i, firstDiff(got, cur))
		}
		versions = append(versions, cur)
		diffs = append(diffs, d)
		prev = cur
	}
	final := versions[K-1]

	// I5: restore at a few points including endpoints.
	for _, p := range []int{1, 1 + rng.Intn(K-1), K} {
		if got := apply(t, base, diffs[:p]); !bytes.Equal(got, versions[p-1]) {
			t.Fatalf("restore p=%d mismatch", p)
		}
	}

	// I2: full merge.
	merged := mergeBytes(t, diffs)
	checkWellFormed(t, merged, uint64(len(final)))
	if got := apply(t, base, [][]byte{merged}); !bytes.Equal(got, final) {
		t.Fatal("full merge mismatch")
	}

	// I6: random contiguous sub-merge spliced back.
	if K >= 3 {
		i := 1 + rng.Intn(K-2)
		j := i + rng.Intn(K-i)
		sub := mergeBytes(t, diffs[i:j+1])
		spliced := make([][]byte, 0, K)
		spliced = append(spliced, diffs[:i]...)
		spliced = append(spliced, sub)
		spliced = append(spliced, diffs[j+1:]...)
		if got := apply(t, base, spliced); !bytes.Equal(got, final) {
			t.Fatalf("sub-merge splice [%d,%d] mismatch", i, j)
		}
	}

	// I4: roll-forward at a random cut.
	cut := 1 + rng.Intn(K-1)
	rb := apply(t, base, diffs[:cut])
	if got := apply(t, rb, diffs[cut:]); !bytes.Equal(got, final) {
		t.Fatalf("roll-forward cut=%d mismatch", cut)
	}

	if !strict {
		return
	}
	if !sawData {
		t.Fatal("no DATA segment produced across the run")
	}
	// ZERO segments require a fully-zero block; the deterministic zero op spans
	// [8KiB,16KiB), so only small block sizes form full zero blocks.
	if blk <= 4096 && !sawZero {
		t.Fatal("no ZERO segment produced across the run")
	}
	for _, op := range []string{"writeRand", "zeroAligned", "shrink", "extendZero", "extendRand", "eofWrite", "zeroTail"} {
		if !st.ops[op] {
			t.Fatalf("op %q never exercised", op)
		}
	}
	// Diffs must carry multiple scattered segments, not just single changes.
	if maxSegs < 3 {
		t.Fatalf("diffs never reached 3+ segments (max %d); multi-modification coverage weak", maxSegs)
	}
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return def
}
