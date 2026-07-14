package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func writeF(t *testing.T, p string, b []byte) {
	t.Helper()
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// makeDiff produces a real content-mode diff file and returns its path.
func makeDiff(t *testing.T, dir, base, newf string) string {
	t.Helper()
	d := filepath.Join(dir, "change")
	if err := cmdDiff([]string{"--mode", "content", "--base", base, "--new", newf, "-o", d}); err != nil {
		t.Fatalf("cmdDiff: %v", err)
	}
	return d
}

// A normal apply must succeed with the default (auto) reflink setting on any
// filesystem -- the CLI default must not force reflink and fail on non-CoW fs.
func TestCLIApplyAutoReflinkRoundTrip(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base")
	newf := filepath.Join(dir, "new")
	baseB := bytes.Repeat([]byte{1}, 8192)
	newB := append([]byte(nil), baseB...)
	copy(newB[100:200], bytes.Repeat([]byte{9}, 100))
	writeF(t, base, baseB)
	writeF(t, newf, newB)

	d := makeDiff(t, dir, base, newf)
	out := filepath.Join(dir, "restored")
	if err := cmdApply([]string{"--base", base, d, "-o", out}); err != nil {
		t.Fatalf("cmdApply (auto reflink) failed: %v", err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, newB) {
		t.Fatal("restored != new")
	}
}

func TestCLIApplyRejectsAliasedDiff(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base")
	newf := filepath.Join(dir, "new")
	writeF(t, base, bytes.Repeat([]byte{1}, 4096))
	writeF(t, newf, bytes.Repeat([]byte{2}, 4096))
	d := makeDiff(t, dir, base, newf)
	orig, _ := os.ReadFile(d)

	if err := cmdApply([]string{"--base", base, d, "-o", d}); err == nil {
		t.Fatal("apply -o aliasing a diff input should be rejected")
	}
	if got, _ := os.ReadFile(d); !bytes.Equal(got, orig) {
		t.Fatal("diff input was clobbered")
	}
}

func TestCLIDiffCheckpointAliasesBase(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base")
	newf := filepath.Join(dir, "new")
	baseB := bytes.Repeat([]byte{7}, 4096)
	writeF(t, base, baseB)
	writeF(t, newf, bytes.Repeat([]byte{8}, 4096))

	// --checkpoint base would clone new over base; must be rejected before that.
	if err := cmdDiff([]string{"--mode", "content", "--base", base, "--new", newf, "--checkpoint", base, "-o", filepath.Join(dir, "change")}); err == nil {
		t.Fatal("diff --checkpoint aliasing base should be rejected")
	}
	if got, _ := os.ReadFile(base); !bytes.Equal(got, baseB) {
		t.Fatal("base was clobbered by checkpoint alias")
	}
}

// resolvedPath must map two paths reaching the same location through a
// symlinked parent directory to the same string, even when the files do not
// exist yet.
func TestResolvedPathSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if resolvedPath(filepath.Join(link, "x")) != resolvedPath(filepath.Join(real, "x")) {
		t.Fatal("resolvedPath did not resolve a symlinked parent to the same path")
	}
}

func TestCLIDiffOutputAliasesCheckpoint(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base")
	newf := filepath.Join(dir, "new")
	writeF(t, base, bytes.Repeat([]byte{7}, 4096))
	writeF(t, newf, bytes.Repeat([]byte{8}, 4096))
	curr := filepath.Join(dir, "curr") // neither -o nor --checkpoint exists yet

	if err := cmdDiff([]string{"--mode", "content", "--base", base, "--new", newf, "--checkpoint", curr, "-o", curr}); err == nil {
		t.Fatal("diff -o aliasing --checkpoint (both nonexistent) should be rejected")
	}
}

// Smoke coverage for the subcommands that previously had no CLI-level tests:
// merge, info, verify (file and stdin), and the usage-error path.
func TestCLIMergeInfoVerifySmoke(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base")
	newf := filepath.Join(dir, "new")
	writeF(t, base, bytes.Repeat([]byte{1}, 4096))
	writeF(t, newf, bytes.Repeat([]byte{2}, 4096))
	d := makeDiff(t, dir, base, newf)

	merged := filepath.Join(dir, "merged")
	if err := cmdMerge([]string{d, "-o", merged}); err != nil {
		t.Fatalf("cmdMerge: %v", err)
	}
	if err := cmdInfo([]string{merged}); err != nil {
		t.Fatalf("cmdInfo: %v", err)
	}
	if err := cmdVerify([]string{merged}); err != nil {
		t.Fatalf("cmdVerify: %v", err)
	}

	f, err := os.Open(merged)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	oldStdin := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = oldStdin }()
	if err := cmdVerify([]string{"-"}); err != nil {
		t.Fatalf("cmdVerify(stdin): %v", err)
	}

	if err := cmdInfo(nil); err == nil {
		t.Fatal("cmdInfo with no args should return a usage error")
	}
	if err := cmdVerify([]string{"a", "b"}); err == nil {
		t.Fatal("cmdVerify with two args should return a usage error")
	}
}
