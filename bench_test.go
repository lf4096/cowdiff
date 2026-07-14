package cowdiff

import (
	"bytes"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// benchPair writes a base file and a "new" file (base with one in-place-sized
// change) into a fresh dir and returns their paths and byte sizes.
func benchPair(b *testing.B, dir string, size, changeLen int) (basePath, newPath string, baseB, newB []byte) {
	b.Helper()
	rng := rand.New(rand.NewSource(1))
	baseB = randBytes(rng, size)
	newB = append([]byte(nil), baseB...)
	off := size/2 - changeLen/2
	rng.Read(newB[off : off+changeLen])
	basePath = filepath.Join(dir, "base")
	newPath = filepath.Join(dir, "new")
	if err := os.WriteFile(basePath, baseB, 0o644); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(newPath, newB, 0o644); err != nil {
		b.Fatal(err)
	}
	return
}

func BenchmarkDiffContent(b *testing.B) {
	const size = 32 << 20
	dir := b.TempDir()
	basePath, newPath, _, _ := benchPair(b, dir, size, 1<<20)
	base, _ := os.Open(basePath)
	defer base.Close()
	nf, _ := os.Open(newPath)
	defer nf.Close()

	b.SetBytes(size)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Diff(base, nf, io.Discard, WithMode(ModeContent)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkApply(b *testing.B) {
	const size = 32 << 20
	dir := b.TempDir()
	basePath, _, baseB, newB := benchPair(b, dir, size, 1<<20)
	d := contentDiffBytesB(b, baseB, newB)
	out := filepath.Join(dir, "out")

	b.SetBytes(size)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		base, _ := os.Open(basePath)
		if err := Apply(base, []io.Reader{bytes.NewReader(d)}, out, WithReflink(false)); err != nil {
			base.Close()
			b.Fatal(err)
		}
		base.Close()
	}
}

func BenchmarkMerge(b *testing.B) {
	const size = 32 << 20
	dir := b.TempDir()
	_, _, baseB, _ := benchPair(b, dir, size, 1<<20)

	// Build a short chain of diffs.
	rng := rand.New(rand.NewSource(2))
	prev := baseB
	var diffs [][]byte
	for i := 0; i < 6; i++ {
		cur := append([]byte(nil), prev...)
		off := rng.Intn(size - 1<<20)
		rng.Read(cur[off : off+1<<20])
		diffs = append(diffs, contentDiffBytesB(b, prev, cur))
		prev = cur
	}

	b.SetBytes(size)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Merge(toReaders(diffs), io.Discard); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseDiff(b *testing.B) {
	const size = 32 << 20
	dir := b.TempDir()
	_, _, baseB, newB := benchPair(b, dir, size, 4<<20)
	d := contentDiffBytesB(b, baseB, newB)

	b.SetBytes(int64(len(d)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := parseDiff(bytes.NewReader(d)); err != nil {
			b.Fatal(err)
		}
	}
}

// The headline comparison: for a large file with a small change, reflink mode
// reads only the changed extents (fast) while content mode reads the whole
// file. Both self-skip when no reflink FS is available (e.g. on darwin).

func BenchmarkDiffReflinkSmallChange(b *testing.B) {
	benchModeSmallChange(b, ModeReflink)
}

func BenchmarkDiffContentSmallChange(b *testing.B) {
	benchModeSmallChange(b, ModeContent)
}

func benchModeSmallChange(b *testing.B, mode Mode) {
	const size = 64 << 20
	dir := requireReflinkB(b)
	rng := rand.New(rand.NewSource(3))
	dp := filepath.Join(dir, "data")
	bp := filepath.Join(dir, "base")
	np := filepath.Join(dir, "new")
	if err := os.WriteFile(dp, randBytes(rng, size), 0o644); err != nil {
		b.Fatal(err)
	}
	if err := Checkpoint(dp, bp); err != nil {
		b.Fatal(err)
	}
	if err := Checkpoint(bp, np); err != nil {
		b.Fatal(err)
	}
	// small in-place change so reflink sees a tiny changed region
	f, _ := os.OpenFile(np, os.O_RDWR, 0)
	f.WriteAt(randBytes(rng, 1<<20), size/2)
	f.Sync()
	f.Close()

	base, _ := os.Open(bp)
	defer base.Close()
	nf, _ := os.Open(np)
	defer nf.Close()

	b.SetBytes(size)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Diff(base, nf, io.Discard, WithMode(mode)); err != nil {
			b.Fatal(err)
		}
	}
}

func contentDiffBytesB(b *testing.B, baseB, newB []byte) []byte {
	b.Helper()
	dir := b.TempDir()
	bp := filepath.Join(dir, "b")
	np := filepath.Join(dir, "n")
	if err := os.WriteFile(bp, baseB, 0o644); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(np, newB, 0o644); err != nil {
		b.Fatal(err)
	}
	base, _ := os.Open(bp)
	defer base.Close()
	nf, _ := os.Open(np)
	defer nf.Close()
	var buf bytes.Buffer
	if _, err := Diff(base, nf, &buf, WithMode(ModeContent)); err != nil {
		b.Fatal(err)
	}
	return buf.Bytes()
}

// requireReflinkB returns a reflink-capable temp dir or skips the benchmark.
func requireReflinkB(b *testing.B) string {
	b.Helper()
	dir := b.TempDir()
	src := filepath.Join(dir, ".probe.src")
	dst := filepath.Join(dir, ".probe.dst")
	if err := os.WriteFile(src, []byte("probe"), 0o644); err != nil {
		b.Fatal(err)
	}
	if err := Checkpoint(src, dst); err != nil {
		b.Skipf("no reflink filesystem under TMPDIR: %v", err)
	}
	os.Remove(src)
	os.Remove(dst)
	return dir
}
