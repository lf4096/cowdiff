# cowdiff

A small tool and Go library for **incremental binary diffs of large files** on copy-on-write (CoW) filesystems.

cowdiff computes a compact, self-contained diff between a base file and a later version, such that `apply(base, diff)` reconstructs the later version byte for byte. When the later version is a reflink clone of base, only the changed extents are read (via `FIEMAP`) -- no full scan; a content-comparison fallback keeps it correct on any filesystem. Diff objects are **storage-agnostic**: they only read and write files and diff streams, compose with pipes, and restore on any machine.

> The reflink fast path works on Linux CoW filesystems: XFS (`reflink=1`), Btrfs, bcachefs.

## Features

- reflink incremental diff: read only the changed extents, no full scan.
- Portable content fallback: any filesystem, cross-machine restore.
- Self-contained, filesystem-independent diff objects with an integrity checksum.
- Chain operations: apply a chain, merge/compact a chain, roll the base forward.
- Sparse / discard aware; handles file grow and shrink correctly.
- Minimal dependency surface (a single external module, `golang.org/x/sys`).

## Install

```sh
go install github.com/lf4096/cowdiff/cmd/cowdiff@latest   # CLI
go get github.com/lf4096/cowdiff                          # library
```

## Concepts

- **base** -- the full baseline of a chain (a plain file), the root of restore.
- **diff** -- an incremental object relative to a single predecessor (base, or the result of the previous diff).
- **chain** -- `base -> D1 -> D2 -> ...`; `apply(base, D1..Dk)` yields the state after Dk.
- **checkpoint** -- a reflink clone produced by the `checkpoint` command, O(1) and almost free in space; used as a base or as the moving baseline for each increment.

## Commands

`-` means stdin/stdout, so any command composes with pipes:

```
cowdiff checkpoint FILE -o OUT                                              # reflink clone + verify shared
cowdiff diff  [--mode reflink|content] --base BASE --new NEW [--checkpoint CKPT] [--from-hash HEX] [-o DIFF|-]
cowdiff apply  --base BASE [--reflink=false] DIFF... -o OUT|-               # diff chain, oldest first
cowdiff merge  DIFF... -o MERGED|-                                          # ordered oldest->newest
cowdiff info   DIFF|-                                                       # header + coverage, no apply
cowdiff verify DIFF|-                                                       # integrity checksum
```

- **checkpoint** -- reflink-clone and verify the extents are really shared (errors instead of silently making a full copy).
- **diff** -- compute the diff of new relative to base. `--checkpoint CKPT` first freezes `--new` into a reflink checkpoint and then diffs against it: the working file may keep changing while the diff is computed, and `CKPT` becomes the baseline for the next increment. `--from-hash` records a caller-supplied whole-file hash in the header for verification (the tool never computes it).
- **apply** -- reconstruct `OUT = base + a chain of diffs` (oldest first). On a reflink filesystem it clones base and patches only the changed regions by default; `--reflink=false` forces copy-then-patch.
- **merge** -- fold an ordered chain of diffs into one, later writes winning; `apply(base, MERGED) == apply(base, the chain)`.

## The two modes

| | `reflink` (default) | `content` |
|---|---|---|
| Finds changes by | comparing physical extents (FIEMAP) | full content comparison |
| Cost | reads only the changed extents | reads both files in full |
| Prerequisite | CoW filesystem + shared reflink lineage | none |

Both modes produce **the same format** and both guarantee `apply(base, diff) == new`, so they can be freely mixed within one chain. `reflink` is the fast path when its prerequisites hold; `content` is the always-available fallback (lineage lost, non-CoW filesystem, a baseline materialized on another machine).

## Typical use: incremental-forever backup

Back a file up incrementally at intervals and compact periodically. Where the diff objects are stored is up to you (wire them to any backend).

Init -- create the level-0 base and the moving baseline:

```sh
cowdiff checkpoint data -o base   # chain root, kept
cowdiff checkpoint data -o prev   # moving baseline, advanced each round, only latest kept
```

Each interval -- append an increment:

```sh
cowdiff diff --base prev --new data --checkpoint curr -o change_i   # freeze data->curr, then diff prev vs curr
mv curr prev                                                        # curr becomes the next baseline
```

Compact periodically -- bound the chain depth and reclaim redundancy:

```sh
cowdiff apply --base base change_1 ... change_j -o base_rolled   # roll the base forward (synthetic full)
cowdiff merge change_{j+1} ... change_{j+m} -o combined          # or merge adjacent diffs, base unchanged
```

Restore (any machine, any retention point p):

```sh
cowdiff apply --base base change_1 ... change_p -o restored
```

This is the classic incremental-forever + synthetic-full pattern: fast restore, bounded chain depth, superseded objects reclaimable. Retention policy, parent bookkeeping, and GC are maintained by your manifest -- the tool only carries an optional `from_hash` in the diff for verification.

## Go library

```go
base, _ := os.Open("base")
newFile, _ := os.Open("data")
out, _ := os.Create("change1")
// reflink mode (default) reads only the changed extents; content mode compares in full and is portable.
_, err := cowdiff.Diff(base, newFile, out, cowdiff.WithMode(cowdiff.ModeContent))

base, _ = os.Open("base")
d, _ := os.Open("change1")
err = cowdiff.Apply(base, []io.Reader{d}, "restored") // base + a chain of diffs -> restored
```

Diff **streams** are `io.Reader` / `io.Writer` (attach to pipes, memory, any storage); the base and new **file** inputs are `*os.File` in both modes. Options use functional options (`With*`). `from_hash` is caller-supplied via `WithFromHash`; the tool never computes it.

```go
// Produce a diff
func Diff(base, newFile *os.File, out io.Writer, opts ...DiffOption) (*Header, error)
//   WithMode(Mode) · WithBlockSize(int) · WithFromHash(string)

// Restore: three layers, choose by output target
func Apply(base *os.File, diffs []io.Reader, targetPath string, opts ...ApplyOption) error // to a file, reflink-accelerated; WithReflink(bool)
func ApplyTo(base io.ReaderAt, diffs []io.Reader, out io.WriterAt) error                   // to a WriterAt, storage-agnostic
func Reconstruct(base io.ReaderAt, diffs []io.Reader, out io.Writer) error                 // sequential, suits stdout / pipes

// Chain operations and inspection
func Merge(diffs []io.Reader, out io.Writer) (*Header, error)
func Checkpoint(srcPath, dstPath string) error
func ReadHeader(r io.Reader) (*Header, error)
func Verify(r io.Reader) error
```

Full documentation at [pkg.go.dev](https://pkg.go.dev/github.com/lf4096/cowdiff).

## Platform

`checkpoint` and `diff --mode reflink` (the default) need a CoW filesystem on Linux (XFS/Btrfs/bcachefs). `diff --mode content`, `apply`, `merge`, `info`, and `verify` work on any platform.

## More

For the object format, two-mode compatibility, consistency, and correctness guarantees, see [DESIGN.md](./DESIGN.md).

## License

Apache-2.0
