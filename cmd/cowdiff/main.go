package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/lf4096/cowdiff"
)

const usage = `cowdiff - reflink-aware incremental binary diff for large files

Usage:
  cowdiff checkpoint FILE -o OUT
  cowdiff diff  [--mode reflink|content] --base BASE --new NEW [--checkpoint CKPT] [--from-hash HEX] [-o DIFF|-]
  cowdiff apply  --base BASE [--reflink=false] DIFF... -o OUT|-
  cowdiff merge  DIFF... -o MERGED|-
  cowdiff info   DIFF|-
  cowdiff verify DIFF|-
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "checkpoint":
		err = cmdCheckpoint(os.Args[2:])
	case "diff":
		err = cmdDiff(os.Args[2:])
	case "apply":
		err = cmdApply(os.Args[2:])
	case "merge":
		err = cmdMerge(os.Args[2:])
	case "info":
		err = cmdInfo(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Fprint(os.Stdout, usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "cowdiff: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// parseArgs parses flags that may be interspersed with positional arguments
// (the stdlib flag package stops at the first positional) and returns the
// collected positionals.
func parseArgs(fs *flag.FlagSet, args []string) []string {
	var positionals []string
	for {
		if err := fs.Parse(args); err != nil {
			return positionals
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positionals
		}
		positionals = append(positionals, rest[0])
		args = rest[1:]
	}
}

func cmdCheckpoint(args []string) error {
	fs := flag.NewFlagSet("checkpoint", flag.ExitOnError)
	out := fs.String("o", "", "output file")
	pos := parseArgs(fs, args)
	if len(pos) != 1 || *out == "" {
		return fmt.Errorf("usage: cowdiff checkpoint FILE -o OUT")
	}
	return cowdiff.Checkpoint(pos[0], *out)
}

func cmdDiff(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ExitOnError)
	mode := fs.String("mode", "reflink", "reflink|content")
	basePath := fs.String("base", "", "base file")
	newPath := fs.String("new", "", "new file")
	ckpt := fs.String("checkpoint", "", "freeze --new into this reflink checkpoint, then diff against it")
	fromHash := fs.String("from-hash", "", "caller-supplied base hash (hex of 32 bytes)")
	out := fs.String("o", "-", "output diff (- for stdout)")
	if extra := parseArgs(fs, args); len(extra) > 0 {
		return fmt.Errorf("cowdiff diff: unexpected argument %q", extra[0])
	}
	if *basePath == "" || *newPath == "" {
		return fmt.Errorf("usage: cowdiff diff [--mode reflink|content] --base BASE --new NEW [--checkpoint CKPT] [--from-hash HEX] [-o DIFF|-]")
	}

	var m cowdiff.Mode
	switch *mode {
	case "reflink":
		m = cowdiff.ModeReflink
	case "content":
		m = cowdiff.ModeContent
	default:
		return fmt.Errorf("cowdiff: unknown mode %q", *mode)
	}

	inputs := []string{*basePath, *newPath}
	if *ckpt != "" {
		inputs = append(inputs, *ckpt)
	}
	if err := checkOutputNotInput(*out, inputs...); err != nil {
		return err
	}

	newForDiff := *newPath
	if *ckpt != "" {
		// The checkpoint is itself an output; it must not alias base or new.
		if err := checkOutputNotInput(*ckpt, *basePath, *newPath); err != nil {
			return err
		}
		if err := cowdiff.Checkpoint(*newPath, *ckpt); err != nil {
			return err
		}
		newForDiff = *ckpt
	}

	base, err := os.Open(*basePath)
	if err != nil {
		return err
	}
	defer base.Close()
	nf, err := os.Open(newForDiff)
	if err != nil {
		return err
	}
	defer nf.Close()

	return writeAtomic(*out, func(w io.Writer) error {
		_, err := cowdiff.Diff(base, nf, w, cowdiff.WithMode(m), cowdiff.WithFromHash(*fromHash))
		return err
	})
}

func cmdApply(args []string) error {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	basePath := fs.String("base", "", "base file")
	reflink := fs.Bool("reflink", true, "use reflink-accelerated apply when possible")
	out := fs.String("o", "", "output file (- for stdout)")
	pos := parseArgs(fs, args)
	if *basePath == "" || *out == "" || len(pos) == 0 {
		return fmt.Errorf("usage: cowdiff apply --base BASE DIFF... -o OUT")
	}

	if err := checkOutputNotInput(*out, append([]string{*basePath}, pos...)...); err != nil {
		return err
	}

	base, err := os.Open(*basePath)
	if err != nil {
		return err
	}
	defer base.Close()

	diffs, closeDiffs, err := openDiffs(pos)
	if err != nil {
		return err
	}
	defer closeDiffs()

	if *out == "-" {
		return cowdiff.Reconstruct(base, diffs, os.Stdout)
	}
	// Default is auto: reflink-accelerate when the filesystem supports it, else
	// copy. Only --reflink=false forces the portable copy path.
	var opts []cowdiff.ApplyOption
	if !*reflink {
		opts = append(opts, cowdiff.WithReflink(false))
	}
	return cowdiff.Apply(base, diffs, *out, opts...)
}

func cmdMerge(args []string) error {
	fs := flag.NewFlagSet("merge", flag.ExitOnError)
	out := fs.String("o", "-", "output merged diff (- for stdout)")
	pos := parseArgs(fs, args)
	if len(pos) == 0 {
		return fmt.Errorf("usage: cowdiff merge DIFF... -o MERGED")
	}

	if err := checkOutputNotInput(*out, pos...); err != nil {
		return err
	}

	diffs, closeDiffs, err := openDiffs(pos)
	if err != nil {
		return err
	}
	defer closeDiffs()

	return writeAtomic(*out, func(w io.Writer) error {
		_, err := cowdiff.Merge(diffs, w)
		return err
	})
}

// writeAtomic runs fn against an output writer. For a file path it writes to a
// temporary sibling and atomically renames on success, so a failure (or a
// mistyped flag caught mid-generation) never truncates or leaves a partial
// output. For "-"/stdout it writes directly.
func writeAtomic(outPath string, fn func(io.Writer) error) error {
	if outPath == "-" || outPath == "" {
		return fn(os.Stdout)
	}
	tmp, err := os.CreateTemp(filepath.Dir(outPath), "."+filepath.Base(outPath)+".cowdiff-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	if err := fn(tmp); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, outPath); err != nil {
		return err
	}
	committed = true
	if d, err := os.Open(filepath.Dir(outPath)); err == nil { // best-effort durability
		d.Sync()
		d.Close()
	}
	return nil
}

// checkOutputNotInput rejects an output path (unless stdout) that names the
// same file as any input, since the finished output would otherwise replace it.
// It compares normalized paths (catching aliases even when the output does not
// exist yet) and, for existing files, inode identity (catching symlink/hard
// link aliases).
func checkOutputNotInput(out string, inputs ...string) error {
	if out == "-" || out == "" {
		return nil
	}
	outResolved := resolvedPath(out)
	oi, _ := os.Stat(out)
	for _, in := range inputs {
		if in == "-" || in == "" {
			continue
		}
		if resolvedPath(in) == outResolved {
			return fmt.Errorf("cowdiff: output %q is the same path as input %q", out, in)
		}
		if oi != nil {
			if ii, err := os.Stat(in); err == nil && os.SameFile(oi, ii) {
				return fmt.Errorf("cowdiff: output %q is the same file as input %q", out, in)
			}
		}
	}
	return nil
}

// resolvedPath returns an absolute path with symlinks resolved on the parent
// directory, so two paths that reach the same file through a symlinked
// directory compare equal even when the file itself does not exist yet.
func resolvedPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if rp, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		return filepath.Join(rp, filepath.Base(abs))
	}
	return abs
}

func cmdInfo(args []string) error {
	path, err := oneArg(args, "info")
	if err != nil {
		return err
	}
	r, closeIn, err := openIn(path)
	if err != nil {
		return err
	}
	defer closeIn()

	h, err := cowdiff.ReadHeader(r)
	if err != nil {
		return err
	}
	var dataBytes, zeroBytes uint64
	var dataSegs, zeroSegs int
	for _, s := range h.Segments {
		if s.Type == cowdiff.SegData {
			dataBytes += s.Length
			dataSegs++
		} else {
			zeroBytes += s.Length
			zeroSegs++
		}
	}
	fmt.Printf("version:      %d\n", h.Version)
	fmt.Printf("target_size:  %d\n", h.TargetSize)
	if h.FromHash != "" {
		fmt.Printf("from_hash:    %s\n", h.FromHash)
	}
	fmt.Printf("segments:     %d (data %d, zero %d)\n", len(h.Segments), dataSegs, zeroSegs)
	fmt.Printf("changed:      %d bytes (data %d, zero %d)\n", dataBytes+zeroBytes, dataBytes, zeroBytes)
	return nil
}

func cmdVerify(args []string) error {
	path, err := oneArg(args, "verify")
	if err != nil {
		return err
	}
	r, closeIn, err := openIn(path)
	if err != nil {
		return err
	}
	defer closeIn()
	if err := cowdiff.Verify(r); err != nil {
		return err
	}
	fmt.Println("OK")
	return nil
}

func oneArg(args []string, cmd string) (string, error) {
	if len(args) != 1 {
		return "", fmt.Errorf("usage: cowdiff %s DIFF|-", cmd)
	}
	return args[0], nil
}

func openIn(path string) (io.Reader, func(), error) {
	if path == "-" {
		return os.Stdin, func() {}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	return f, func() { f.Close() }, nil
}

func openDiffs(paths []string) ([]io.Reader, func(), error) {
	readers := make([]io.Reader, 0, len(paths))
	closers := make([]func(), 0, len(paths))
	closeAll := func() {
		for _, c := range closers {
			c()
		}
	}
	for _, p := range paths {
		r, c, err := openIn(p)
		if err != nil {
			closeAll()
			return nil, nil, err
		}
		readers = append(readers, r)
		closers = append(closers, c)
	}
	return readers, closeAll, nil
}
