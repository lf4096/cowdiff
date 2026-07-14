//go:build linux

package cowdiff

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// requireReflink returns a temp dir on a reflink-capable filesystem, or skips.
// If COWDIFF_REQUIRE_REFLINK=1, an unavailable reflink FS is a hard failure so
// a misconfigured run cannot report green with zero reflink coverage.
func requireReflink(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, ".probe.src")
	dst := filepath.Join(dir, ".probe.dst")
	if err := os.WriteFile(src, []byte("probe"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Checkpoint(src, dst); err != nil {
		if os.Getenv("COWDIFF_REQUIRE_REFLINK") == "1" {
			t.Fatalf("COWDIFF_REQUIRE_REFLINK=1 but no reflink FS under TMPDIR (%s): %v", dir, err)
		}
		t.Skipf("no reflink filesystem under TMPDIR (%s): %v", dir, err)
	}
	os.Remove(src)
	os.Remove(dst)
	return dir
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// patchInPlace CoW-writes bytes at off without disturbing sharing of the rest.
func patchInPlace(t *testing.T, path string, off int64, data []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(data, off); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func reflinkDiff(t *testing.T, basePath, newPath string) []byte {
	t.Helper()
	base, err := os.Open(basePath)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	nf, err := os.Open(newPath)
	if err != nil {
		t.Fatal(err)
	}
	defer nf.Close()
	var buf bytes.Buffer
	if _, err := Diff(base, nf, &buf, WithMode(ModeReflink)); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestCheckpoint(t *testing.T) {
	dir := requireReflink(t)
	rng := rand.New(rand.NewSource(50))
	data := randBytes(rng, 8*1024*1024)
	dp := filepath.Join(dir, "data")
	bp := filepath.Join(dir, "base")
	writeFileT(t, dp, data)
	if err := Checkpoint(dp, bp); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, bp); !bytes.Equal(got, data) {
		t.Fatal("checkpoint content mismatch")
	}

	// Extents must be physically shared (this is what makes reflink diff cheap).
	df, _ := os.Open(dp)
	defer df.Close()
	bf, _ := os.Open(bp)
	defer bf.Close()
	de, err := fiemapExtents(df)
	if err != nil {
		t.Fatal(err)
	}
	be, err := fiemapExtents(bf)
	if err != nil {
		t.Fatal(err)
	}
	if len(de) == 0 || len(de) != len(be) {
		t.Fatalf("extent count %d vs %d", len(de), len(be))
	}
	for i := range de {
		if de[i].physical != be[i].physical || de[i].length != be[i].length {
			t.Fatalf("extent %d not shared: phys %d/%d", i, de[i].physical, be[i].physical)
		}
	}
}

// Checkpoint must refuse to clone onto its own source (same path or a hard
// link) rather than truncating it, and must not disturb an existing dst on a
// same-file rejection.
func TestCheckpointRejectsSameFile(t *testing.T) {
	dir := requireReflink(t)
	rng := rand.New(rand.NewSource(56))
	src := filepath.Join(dir, "src")
	orig := randBytes(rng, 1<<20)
	writeFileT(t, src, orig)

	if err := Checkpoint(src, src); err == nil {
		t.Fatal("Checkpoint(src, src) should be rejected")
	}
	link := filepath.Join(dir, "hardlink")
	if err := os.Link(src, link); err != nil {
		t.Fatalf("hard link: %v", err)
	}
	if err := Checkpoint(src, link); err == nil {
		t.Fatal("Checkpoint onto a hard link of src should be rejected")
	}
	if got := mustRead(t, src); !bytes.Equal(got, orig) {
		t.Fatal("source was modified by a rejected checkpoint")
	}

	// A normal checkpoint over a distinct existing file replaces it atomically.
	dst := filepath.Join(dir, "dst")
	writeFileT(t, dst, []byte("stale"))
	if err := Checkpoint(src, dst); err != nil {
		t.Fatalf("checkpoint over existing file: %v", err)
	}
	if got := mustRead(t, dst); !bytes.Equal(got, orig) {
		t.Fatal("checkpoint did not replace existing dst with source content")
	}
}

// extReliable must treat inline/tail/not-aligned layouts as unreliable, so a
// changed inline (e.g. Btrfs) file whose physical offset is 0 is not mistaken
// for unchanged.
func TestExtReliable(t *testing.T) {
	if !extReliable(0) {
		t.Fatal("a plain aligned extent must be reliable")
	}
	for _, f := range []uint32{
		fiemapExtentNotAligned, fiemapExtentDataInline, fiemapExtentDataTail,
		fiemapExtentUnknown, fiemapExtentDelalloc, fiemapExtentEncoded, fiemapExtentDataEncrypted,
	} {
		if extReliable(f) {
			t.Fatalf("extent flag 0x%x must be unreliable", f)
		}
	}
}

// TestReflinkFuzz gives the reflink/FIEMAP path the same random breadth as the
// content-mode fuzzer: many random chains, each round reflink-cloning the
// previous version, applying scattered in-place patches (so sharing is
// preserved elsewhere), and diffing via ModeReflink over real FICLONE'd files.
// Count is COWDIFF_FUZZ_REFLINK (default 20); skipped off a reflink filesystem.
func TestReflinkFuzz(t *testing.T) {
	requireReflink(t) // probe; skip whole test off a CoW fs
	for i := 0; i < envInt("COWDIFF_FUZZ_REFLINK", 20); i++ {
		i := i
		t.Run(fmt.Sprintf("reflinkfuzz%d", i), func(t *testing.T) {
			t.Parallel()
			runReflinkChain(t, int64(2000+i))
		})
	}
}

func runReflinkChain(t *testing.T, seed int64) {
	dir := requireReflink(t)
	rng := rand.New(rand.NewSource(seed))
	size := 4*1024*1024 + rng.Intn(20*1024*1024) // 4-24 MiB
	basePath := filepath.Join(dir, "base")
	writeFileT(t, basePath, randBytes(rng, size))
	baseB := mustRead(t, basePath)

	prevPath, prevB := basePath, baseB
	lastB := baseB
	var diffs [][]byte
	K := 2 + rng.Intn(6)
	for r := 0; r < K; r++ {
		curPath := filepath.Join(dir, fmt.Sprintf("v%d", r))
		if err := Checkpoint(prevPath, curPath); err != nil {
			t.Fatal(err)
		}
		for p := 0; p < 1+rng.Intn(4); p++ {
			off := int64(rng.Intn(size - 256*1024))
			n := 32*1024 + rng.Intn(224*1024)
			patchInPlace(t, curPath, off, randBytes(rng, n))
		}
		curB := mustRead(t, curPath)
		d := reflinkDiff(t, prevPath, curPath)
		checkWellFormed(t, d, uint64(len(curB)))
		if got := reconstructMem(t, prevB, [][]byte{d}); !bytes.Equal(got, curB) {
			t.Fatalf("round %d reflink round-trip mismatch", r)
		}
		diffs = append(diffs, d)
		prevPath, prevB, lastB = curPath, curB, curB
	}
	if got := reconstructMem(t, baseB, diffs); !bytes.Equal(got, lastB) {
		t.Fatal("reflink chain mismatch")
	}
	merged := mergeBytes(t, diffs)
	if got := reconstructMem(t, baseB, [][]byte{merged}); !bytes.Equal(got, lastB) {
		t.Fatal("reflink merge mismatch")
	}
}

func TestReflinkDiffRoundTrip(t *testing.T) {
	dir := requireReflink(t)
	rng := rand.New(rand.NewSource(51))
	const fileSize = 48 * 1024 * 1024
	dp := filepath.Join(dir, "data")
	bp := filepath.Join(dir, "base")
	np := filepath.Join(dir, "new")
	writeFileT(t, dp, randBytes(rng, fileSize))
	if err := Checkpoint(dp, bp); err != nil {
		t.Fatal(err)
	}
	if err := Checkpoint(bp, np); err != nil { // new shares extents with base
		t.Fatal(err)
	}

	// Modify only a small region IN PLACE so sharing elsewhere is preserved.
	const patchOff, patchLen = 10 * 1024 * 1024, 2 * 1024 * 1024
	patchInPlace(t, np, patchOff, randBytes(rng, patchLen))

	baseB := mustRead(t, bp)
	newB := mustRead(t, np)
	d := reflinkDiff(t, bp, np)
	checkWellFormed(t, d, uint64(len(newB)))

	// Correctness: round-trips to new via all three apply methods.
	if got := applyAll(t, baseB, [][]byte{d}); !bytes.Equal(got, newB) {
		t.Fatalf("reflink round-trip mismatch at byte %d", firstDiff(got, newB))
	}

	// F1 minimality: the diff must NOT cover the whole file. A broken classify
	// that reports everything changed would still round-trip, so assert the
	// diff is small and a far untouched block is covered by no segment.
	h, _ := ReadHeader(bytes.NewReader(d))
	var covered uint64
	for _, s := range h.Segments {
		covered += s.Length
	}
	if covered > uint64(fileSize)/4 {
		t.Fatalf("reflink diff not minimal: covered %d of %d bytes (over-reporting?)", covered, fileSize)
	}
	if s := segmentAt(h, 30*1024*1024); s != nil {
		t.Fatalf("untouched block covered by reflink diff: %+v", s)
	}

	// F4: reflink-accelerated apply (forced clone+patch) must also equal new.
	baseF, _ := os.Open(bp)
	defer baseF.Close()
	outP := filepath.Join(dir, "out.reflink")
	if err := Apply(baseF, toReaders([][]byte{d}), outP, WithReflink(true)); err != nil {
		t.Fatalf("reflink-accelerated apply: %v", err)
	}
	if got := mustRead(t, outP); !bytes.Equal(got, newB) {
		t.Fatal("reflink-accelerated apply != new")
	}
}

// TestModeEquivalence: reflink and content diffs of the same pair both
// reconstruct new (I7), though their segment bytes may differ.
func TestModeEquivalence(t *testing.T) {
	dir := requireReflink(t)
	rng := rand.New(rand.NewSource(52))
	const fileSize = 24 * 1024 * 1024
	dp := filepath.Join(dir, "data")
	bp := filepath.Join(dir, "base")
	np := filepath.Join(dir, "new")
	writeFileT(t, dp, randBytes(rng, fileSize))
	if err := Checkpoint(dp, bp); err != nil {
		t.Fatal(err)
	}
	if err := Checkpoint(bp, np); err != nil {
		t.Fatal(err)
	}
	patchInPlace(t, np, 4*1024*1024, randBytes(rng, 1024*1024))
	patchInPlace(t, np, 20*1024*1024, randBytes(rng, 512*1024))

	baseB := mustRead(t, bp)
	newB := mustRead(t, np)
	dr := reflinkDiff(t, bp, np)
	dc := contentDiffBytes(t, baseB, newB)

	gr := applyAll(t, baseB, [][]byte{dr})
	gc := applyAll(t, baseB, [][]byte{dc})
	if !bytes.Equal(gr, newB) {
		t.Fatal("reflink diff does not reconstruct new")
	}
	if !bytes.Equal(gc, newB) {
		t.Fatal("content diff does not reconstruct new")
	}
	if !bytes.Equal(gr, gc) {
		t.Fatal("reflink and content applied results differ")
	}
}

// TestAllocatedZeroDivergence (F8): an ALLOCATED zero region is stored as DATA
// by reflink mode (literal zeros) but as ZERO by content mode. Both reconstruct
// the same result -- pinning the documented interchangeability.
func TestAllocatedZeroDivergence(t *testing.T) {
	dir := requireReflink(t)
	rng := rand.New(rand.NewSource(53))
	const fileSize = 16 * 1024 * 1024
	dp := filepath.Join(dir, "data")
	bp := filepath.Join(dir, "base")
	np := filepath.Join(dir, "new")
	writeFileT(t, dp, randBytes(rng, fileSize))
	if err := Checkpoint(dp, bp); err != nil {
		t.Fatal(err)
	}
	if err := Checkpoint(bp, np); err != nil {
		t.Fatal(err)
	}
	// Write REAL zero bytes (allocated, not a hole) over [4MiB, 8MiB).
	const zoff, zlen = 4 * 1024 * 1024, 4 * 1024 * 1024
	patchInPlace(t, np, zoff, make([]byte, zlen))

	baseB := mustRead(t, bp)
	newB := mustRead(t, np)
	dr := reflinkDiff(t, bp, np)
	dc := contentDiffBytes(t, baseB, newB)

	hr, _ := ReadHeader(bytes.NewReader(dr))
	hc, _ := ReadHeader(bytes.NewReader(dc))
	if s := segmentAt(hr, zoff+zlen/2); s == nil || s.Type != SegData {
		t.Fatalf("reflink: allocated-zero region should be DATA, got %+v", s)
	}
	if s := segmentAt(hc, zoff+zlen/2); s == nil || s.Type != SegZero {
		t.Fatalf("content: zero region should be ZERO, got %+v", s)
	}
	if got := applyAll(t, baseB, [][]byte{dr}); !bytes.Equal(got, newB) {
		t.Fatal("reflink apply mismatch")
	}
	if got := applyAll(t, baseB, [][]byte{dc}); !bytes.Equal(got, newB) {
		t.Fatal("content apply mismatch")
	}
}

// TestMixedModeChain: a chain that alternates reflink and content diffs applies
// and merges correctly.
func TestMixedModeChain(t *testing.T) {
	dir := requireReflink(t)
	rng := rand.New(rand.NewSource(54))
	const fileSize = 16 * 1024 * 1024

	v0p := filepath.Join(dir, "v0")
	writeFileT(t, v0p, randBytes(rng, fileSize))
	basePath := filepath.Join(dir, "base")
	if err := Checkpoint(v0p, basePath); err != nil {
		t.Fatal(err)
	}
	baseB := mustRead(t, basePath)

	prevPath := basePath
	prevB := baseB
	var diffs [][]byte
	var lastB []byte
	for i := 0; i < 6; i++ {
		curPath := filepath.Join(dir, "v"+string(rune('a'+i)))
		if err := Checkpoint(prevPath, curPath); err != nil {
			t.Fatal(err)
		}
		patchInPlace(t, curPath, int64(rng.Intn(fileSize-1024*1024)), randBytes(rng, 512*1024))
		curB := mustRead(t, curPath)
		var d []byte
		if i%2 == 0 {
			d = reflinkDiff(t, prevPath, curPath)
		} else {
			d = contentDiffBytes(t, prevB, curB)
		}
		checkWellFormed(t, d, uint64(len(curB)))
		diffs = append(diffs, d)
		prevPath, prevB, lastB = curPath, curB, curB
	}

	if got := applyAll(t, baseB, diffs); !bytes.Equal(got, lastB) {
		t.Fatal("mixed-mode chain apply mismatch")
	}
	merged := mergeBytes(t, diffs)
	if got := applyAll(t, baseB, [][]byte{merged}); !bytes.Equal(got, lastB) {
		t.Fatal("mixed-mode merge apply mismatch")
	}
}

// TestReflinkPropertyChain: several rounds of in-place-mutated reflink clones,
// asserting round-trip and minimality each round, then chain + merge.
func TestReflinkPropertyChain(t *testing.T) {
	dir := requireReflink(t)
	rng := rand.New(rand.NewSource(55))
	const fileSize = 16 * 1024 * 1024

	basePath := filepath.Join(dir, "base")
	writeFileT(t, basePath, randBytes(rng, fileSize))
	baseB := mustRead(t, basePath)

	prevPath, prevB := basePath, baseB
	var diffs [][]byte
	var lastB []byte
	const N = 8
	for i := 0; i < N; i++ {
		curPath := filepath.Join(dir, "c"+string(rune('a'+i)))
		if err := Checkpoint(prevPath, curPath); err != nil {
			t.Fatal(err)
		}
		// a few small in-place patches (preserves sharing elsewhere)
		var touched uint64
		for k := 0; k < 3; k++ {
			off := int64(rng.Intn(fileSize - 1024*1024))
			n := 64*1024 + rng.Intn(256*1024)
			patchInPlace(t, curPath, off, randBytes(rng, n))
			touched += uint64(n)
		}
		curB := mustRead(t, curPath)
		d := reflinkDiff(t, prevPath, curPath)
		checkWellFormed(t, d, uint64(len(curB)))
		if got := applyAll(t, prevB, [][]byte{d}); !bytes.Equal(got, curB) {
			t.Fatalf("round %d reflink round-trip mismatch", i)
		}
		// minimality: covered no more than a generous multiple of touched bytes
		h, _ := ReadHeader(bytes.NewReader(d))
		var covered uint64
		for _, s := range h.Segments {
			covered += s.Length
		}
		if covered > uint64(fileSize)/2 {
			t.Fatalf("round %d reflink diff not minimal: covered %d of %d", i, covered, fileSize)
		}
		diffs = append(diffs, d)
		prevPath, prevB, lastB = curPath, curB, curB
	}

	if got := applyAll(t, baseB, diffs); !bytes.Equal(got, lastB) {
		t.Fatal("reflink property chain apply mismatch")
	}
	merged := mergeBytes(t, diffs)
	if got := applyAll(t, baseB, [][]byte{merged}); !bytes.Equal(got, lastB) {
		t.Fatal("reflink property chain merge mismatch")
	}
}

func TestNormalizeExtents(t *testing.T) {
	split := []extent{
		{logical: 0, physical: 4096, length: 4096},
		{logical: 4096, physical: 8192, length: 8192},
		{logical: 16384, physical: 65536, length: 4096},
	}
	merged := []extent{
		{logical: 0, physical: 4096, length: 12288},
		{logical: 16384, physical: 65536, length: 4096},
	}
	ns, nm := normalizeExtents(split), normalizeExtents(merged)
	if len(ns) != len(nm) {
		t.Fatalf("normalized lengths differ: %d != %d", len(ns), len(nm))
	}
	for i := range ns {
		if ns[i].logical != nm[i].logical || ns[i].physical != nm[i].physical || ns[i].length != nm[i].length {
			t.Fatalf("normalized extent %d differs: %+v != %+v", i, ns[i], nm[i])
		}
	}

	// Logically adjacent but physically discontiguous extents must not merge.
	gap := []extent{
		{logical: 0, physical: 4096, length: 4096},
		{logical: 4096, physical: 65536, length: 4096},
	}
	if got := normalizeExtents(gap); len(got) != 2 {
		t.Fatalf("physically discontiguous extents merged: %+v", got)
	}

	// A different physical mapping stays different after normalization.
	other := []extent{{logical: 0, physical: 131072, length: 12288}, {logical: 16384, physical: 65536, length: 4096}}
	no := normalizeExtents(other)
	if no[0].physical == ns[0].physical {
		t.Fatal("distinct mappings compared equal")
	}
}

// TestCheckpointHeavyDirty exercises checkpoint against a source with a large
// amount of unflushed delalloc state, the situation where FIEMAP views of the
// clone pair can transiently disagree on extent boundaries.
func TestCheckpointHeavyDirty(t *testing.T) {
	dir := requireReflink(t)
	src := filepath.Join(dir, "dirty.src")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	rnd := rand.New(rand.NewSource(7))
	buf := make([]byte, 1<<20)
	for round := 0; round < 4; round++ {
		// Scattered 1MiB writes across a 256MiB range, never fsynced.
		for i := 0; i < 32; i++ {
			rnd.Read(buf)
			off := int64(rnd.Intn(256)) << 20
			if _, err := f.WriteAt(buf, off); err != nil {
				t.Fatal(err)
			}
		}
		dst := filepath.Join(dir, fmt.Sprintf("dirty.ckpt%d", round))
		if err := Checkpoint(src, dst); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		got, err := os.ReadFile(dst)
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("round %d: checkpoint content diverges from source", round)
		}
	}
}

func TestExtentsCovered(t *testing.T) {
	src := []extent{
		{logical: 0, physical: 4096, length: 12288},
		// src-only speculative preallocation tail beyond what dst shares.
		{logical: 16384, physical: 65536, length: 8192},
	}
	ok := [][]extent{
		{{logical: 0, physical: 4096, length: 12288}, {logical: 16384, physical: 65536, length: 4096}},
		// dst reported split where src is merged.
		{{logical: 0, physical: 4096, length: 4096}, {logical: 8192, physical: 12288, length: 4096}},
		nil,
	}
	for i, d := range ok {
		if err := extentsCovered(src, d); err != nil {
			t.Fatalf("case %d should be covered: %v", i, err)
		}
	}
	bad := [][]extent{
		// different physical block
		{{logical: 0, physical: 131072, length: 4096}},
		// logical range src does not map
		{{logical: 32768, physical: 4096, length: 4096}},
		// extends past the end of src's mapping
		{{logical: 16384, physical: 65536, length: 16384}},
	}
	for i, d := range bad {
		if err := extentsCovered(src, d); err == nil {
			t.Fatalf("case %d should fail coverage", i)
		}
	}
}
