package cowdiff_test

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/lf4096/cowdiff"
)

// Example diffs one version of a file against another and applies the diff to
// reconstruct it. It uses content mode; the default reflink mode additionally
// requires a copy-on-write filesystem and reflink-shared inputs.
func Example() {
	dir, _ := os.MkdirTemp("", "cowdiff-example")
	defer os.RemoveAll(dir)

	base := filepath.Join(dir, "base")
	updated := filepath.Join(dir, "new")
	os.WriteFile(base, []byte("the quick brown fox"), 0o644)
	os.WriteFile(updated, []byte("the quick RED fox!!"), 0o644)

	// Diff updated against base into an in-memory object.
	bf, _ := os.Open(base)
	defer bf.Close()
	nf, _ := os.Open(updated)
	defer nf.Close()

	var diff bytes.Buffer
	if _, err := cowdiff.Diff(bf, nf, &diff, cowdiff.WithMode(cowdiff.ModeContent)); err != nil {
		panic(err)
	}

	// Apply base + diff to reconstruct the updated file.
	bf2, _ := os.Open(base)
	defer bf2.Close()
	restored := filepath.Join(dir, "restored")
	if err := cowdiff.Apply(bf2, []io.Reader{&diff}, restored); err != nil {
		panic(err)
	}

	out, _ := os.ReadFile(restored)
	fmt.Printf("%s\n", out)
	// Output: the quick RED fox!!
}
