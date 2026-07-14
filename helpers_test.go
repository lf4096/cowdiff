package cowdiff

import (
	"bytes"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// model is an in-memory image that the tests diff against as ground truth.
type model struct{ b []byte }

func (m *model) bytes() []byte { return append([]byte(nil), m.b...) }
func (m *model) size() int     { return len(m.b) }

func (m *model) grow(n int) {
	if n > len(m.b) {
		m.b = append(m.b, make([]byte, n-len(m.b))...)
	}
}

func (m *model) writeAt(off int, data []byte) {
	if end := off + len(data); end > len(m.b) {
		m.grow(end)
	}
	copy(m.b[off:], data)
}

func (m *model) zeroAt(off, n int) {
	if end := off + n; end > len(m.b) {
		m.grow(end)
	}
	for i := off; i < off+n; i++ {
		m.b[i] = 0
	}
}

// truncate shrinks or zero-extends to n.
func (m *model) truncate(n int) {
	if n <= len(m.b) {
		m.b = append([]byte(nil), m.b[:n]...)
		return
	}
	m.grow(n)
}

func randBytes(rng *rand.Rand, n int) []byte {
	b := make([]byte, n)
	rng.Read(b)
	return b
}

func writeFileT(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func openRO(t *testing.T, path string) (*os.File, error) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return f, nil
}

func toReaders(diffs [][]byte) []io.Reader {
	rs := make([]io.Reader, len(diffs))
	for i, d := range diffs {
		rs[i] = bytes.NewReader(d)
	}
	return rs
}

// contentDiffBytes produces a content-mode diff of newB relative to baseB.
func contentDiffBytes(t *testing.T, baseB, newB []byte, opts ...DiffOption) []byte {
	t.Helper()
	dir := t.TempDir()
	bp := filepath.Join(dir, "b")
	np := filepath.Join(dir, "n")
	writeFileT(t, bp, baseB)
	writeFileT(t, np, newB)
	base, err := os.Open(bp)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	nf, err := os.Open(np)
	if err != nil {
		t.Fatal(err)
	}
	defer nf.Close()

	var buf bytes.Buffer
	all := append([]DiffOption{WithMode(ModeContent)}, opts...)
	if _, err := Diff(base, nf, &buf, all...); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func mergeBytes(t *testing.T, diffs [][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := Merge(toReaders(diffs), &buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func lastTargetSize(t *testing.T, diffs [][]byte) uint64 {
	t.Helper()
	h, err := ReadHeader(bytes.NewReader(diffs[len(diffs)-1]))
	if err != nil {
		t.Fatal(err)
	}
	return h.TargetSize
}

// fillSentinel makes f exactly size bytes of 0xff, so a later ApplyTo that
// drops a region leaves a detectable sentinel rather than a masking zero.
func fillSentinel(t *testing.T, f *os.File, size int64) {
	t.Helper()
	buf := bytes.Repeat([]byte{0xff}, 1<<16)
	for off := int64(0); off < size; {
		n := int64(len(buf))
		if off+n > size {
			n = size - off
		}
		if _, err := f.WriteAt(buf[:n], off); err != nil {
			t.Fatal(err)
		}
		off += n
	}
}

// applyAll reconstructs baseB + diffs three independent ways (Apply via the
// per-diff streaming path; ApplyTo and Reconstruct via the chain-folding path),
// asserts all three agree, and returns the result. Requires len(diffs) >= 1.
func applyAll(t *testing.T, baseB []byte, diffs [][]byte) []byte {
	t.Helper()
	if len(diffs) == 0 {
		t.Fatal("applyAll requires at least one diff")
	}
	dir := t.TempDir()
	bp := filepath.Join(dir, "base")
	writeFileT(t, bp, baseB)
	finalSize := int64(lastTargetSize(t, diffs))

	// 1. Apply -> file (copy fallback, per-diff streaming path).
	baseF, err := os.Open(bp)
	if err != nil {
		t.Fatal(err)
	}
	outP := filepath.Join(dir, "apply")
	if err := Apply(baseF, toReaders(diffs), outP, WithReflink(false)); err != nil {
		baseF.Close()
		t.Fatalf("Apply: %v", err)
	}
	baseF.Close()
	applyRes, err := os.ReadFile(outP)
	if err != nil {
		t.Fatal(err)
	}

	// 2. ApplyTo -> WriterAt pre-filled with 0xff (F5: catch dropped writes).
	atP := filepath.Join(dir, "applyto")
	atF, err := os.Create(atP)
	if err != nil {
		t.Fatal(err)
	}
	fillSentinel(t, atF, finalSize)
	baseRA, err := os.Open(bp)
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyTo(baseRA, toReaders(diffs), atF); err != nil {
		baseRA.Close()
		atF.Close()
		t.Fatalf("ApplyTo: %v", err)
	}
	baseRA.Close()
	atF.Close()
	atRes, err := os.ReadFile(atP)
	if err != nil {
		t.Fatal(err)
	}

	// 3. Reconstruct -> sequential Writer.
	baseRA2, err := os.Open(bp)
	if err != nil {
		t.Fatal(err)
	}
	var seq bytes.Buffer
	if err := Reconstruct(baseRA2, toReaders(diffs), &seq); err != nil {
		baseRA2.Close()
		t.Fatalf("Reconstruct: %v", err)
	}
	baseRA2.Close()
	recRes := seq.Bytes()

	if !bytes.Equal(applyRes, atRes) {
		t.Fatalf("Apply != ApplyTo: %d vs %d bytes; first diff at %d", len(applyRes), len(atRes), firstDiff(applyRes, atRes))
	}
	if !bytes.Equal(applyRes, recRes) {
		t.Fatalf("Apply != Reconstruct: %d vs %d bytes; first diff at %d", len(applyRes), len(recRes), firstDiff(applyRes, recRes))
	}
	return applyRes
}

// reconstructMem reconstructs base+diffs in memory via Reconstruct and ApplyTo
// (both resolveChain-based) and asserts they agree. It is an fsync-free, file-
// free round-trip check for the breadth fuzz; signature matches applyAll.
func reconstructMem(t *testing.T, baseB []byte, diffs [][]byte) []byte {
	t.Helper()
	var seq bytes.Buffer
	if err := Reconstruct(bytes.NewReader(baseB), toReaders(diffs), &seq); err != nil {
		t.Fatalf("Reconstruct: %v", err)
	}
	finalSize := int64(lastTargetSize(t, diffs))
	w := &sliceWriterAt{b: bytes.Repeat([]byte{0xff}, int(finalSize))}
	if err := ApplyTo(bytes.NewReader(baseB), toReaders(diffs), w); err != nil {
		t.Fatalf("ApplyTo: %v", err)
	}
	if !bytes.Equal(seq.Bytes(), w.b) {
		t.Fatalf("Reconstruct != ApplyTo: %d vs %d bytes; first diff at %d", seq.Len(), len(w.b), firstDiff(seq.Bytes(), w.b))
	}
	return seq.Bytes()
}

func firstDiff(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	if len(a) != len(b) {
		return n
	}
	return -1
}

// checkWellFormed asserts diff-object structural invariants (I9): correct
// target size, segments sorted, non-overlapping, in-bounds, non-empty, adjacent
// same-type runs coalesced (F6), and a passing integrity checksum.
func checkWellFormed(t *testing.T, diffBytes []byte, wantSize uint64) {
	t.Helper()
	h, err := ReadHeader(bytes.NewReader(diffBytes))
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	if h.TargetSize != wantSize {
		t.Fatalf("TargetSize=%d want %d", h.TargetSize, wantSize)
	}
	var prevEnd uint64
	for i, s := range h.Segments {
		if s.Length == 0 {
			t.Fatalf("seg %d has zero length", i)
		}
		if s.Offset < prevEnd {
			t.Fatalf("seg %d unsorted/overlapping: offset %d < prevEnd %d", i, s.Offset, prevEnd)
		}
		if s.Offset+s.Length > h.TargetSize {
			t.Fatalf("seg %d out of bounds: %d+%d > %d", i, s.Offset, s.Length, h.TargetSize)
		}
		if i > 0 {
			p := h.Segments[i-1]
			if p.Type == s.Type && p.Offset+p.Length == s.Offset {
				t.Fatalf("segs %d,%d adjacent and same-type: not coalesced", i-1, i)
			}
		}
		prevEnd = s.Offset + s.Length
	}
	if err := Verify(bytes.NewReader(diffBytes)); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// segmentAt returns the segment covering off, or nil.
func segmentAt(h *Header, off uint64) *Segment {
	for i := range h.Segments {
		s := &h.Segments[i]
		if s.Offset <= off && off < s.Offset+s.Length {
			return s
		}
	}
	return nil
}
