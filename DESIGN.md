# cowdiff - Design

This document describes cowdiff's object format, operation semantics, and design rationale. For installation, commands, and typical usage see the [README](./README.md); for the full API see [godoc](https://pkg.go.dev/github.com/lf4096/cowdiff).

## 1. What it solves

Given a large file `B` (base) and a later version `N`, produce a compact **diff object** `D` such that `apply(B, D)` reconstructs `N` byte for byte. When `N` is a reflink clone of `B`, only the changed regions are read (via `FIEMAP`) -- no full scan -- and the result is **always correct**.

This is the primitive behind incremental backup, snapshot chains, and cross-machine restore -- without relying on a filesystem's native `send`/`receive`, and without a full scan per snapshot.

## 2. Goals / non-goals

**Goals**

- Cheap incremental diff of reflink descendants (read only the changed extents via `FIEMAP`, no full scan).
- A correct, portable fallback (content comparison) when reflink lineage is unavailable.
- Self-contained diff objects: `apply(base, diff)` is deterministic and filesystem-independent.
- Chain operations: apply a chain, merge/compact a chain.
- Reflink-accelerated apply (clone base, patch only the changed regions).
- A minimal dependency surface; usable both as a CLI (Unix filter) and as a Go library.

**Non-goals**

- **Storage and transport.** The tool reads and writes files and diff streams; wiring them to any backend is the caller's job.
- **Chain bookkeeping.** Parent relationships, retention policy, garbage collection, fork/branch reachability -- that is a manifest the caller maintains. A diff object carries only an optional `from_hash` for **verification**.
- **Lazy read-through.** Restore materializes the full file. If you need to run directly on a backing chain without materializing, use a format built for that (qcow2 backing chain, or a FUSE overlay).
- **Online locking / consistency.** The tool assumes its input files are quiescent (see section 7); it does not freeze or lock.

## 3. Concepts

- **base** -- the level-0 baseline of a chain: the full content of the working file at the chain's start (a plain file), the root of restore. It has no parent.
- **diff** -- an incremental object relative to a single predecessor (the base, or the result of another diff).
- **chain** -- `base -> D1 -> D2 -> ... -> Dk`. `apply(base, D1..Dk)` yields the state after `Dk`.
- **checkpoint** -- a reflink clone (see section 6), O(1) and almost free in space. It is used both to build a base (kept as the chain root, stored) and as the moving baseline for each increment (local, only the latest kept).
- **mode** -- `reflink` (the FIEMAP fast path) and `content` (the portable fallback); both produce the same format (see section 5).

## 4. Diff object format

A diff object is a single self-contained binary blob laid out so that **the directory precedes the data**. This lets a consumer cheaply read all the headers of a chain, work out which layer each byte range comes from, and then fetch only the data it actually needs.

```
+-----------------------------------------------------------+
| Header                                                    |
|   magic         8   "COWDIFF\x01"                         |
|   version       u32                                       |
|   flags         u32   bit0 has_from_hash                  |
|   target_size   u64   file size after apply (grow/shrink) |
|   seg_count     u64                                       |
|   from_hash     32?   full-file hash the diff applies onto|
+-----------------------------------------------------------+
| Segment directory (seg_count entries, 25 bytes each)      |
|   offset        u64   logical offset in the target        |
|   length        u64                                       |
|   type          u8    0 = DATA, 1 = ZERO (hole/discard)   |
|   data_off      u64   byte offset into the Data section   |
|                       (unused for ZERO segments)          |
+-----------------------------------------------------------+
| Data section                                              |
|   concatenated bytes of all DATA segments, in directory   |
|   order                                                   |
+-----------------------------------------------------------+
| Trailer                                                   |
|   checksum      32   SHA-256 of header + directory + data |
+-----------------------------------------------------------+
```

All integers are little-endian. Design points and their reasons:

- **Directory before data** -- headers stream cheaply, restore can be optimized (skip lower layers that are fully overwritten), and only the needed data ranges are fetched.
- **Segment `type` DATA / ZERO** -- trim/discard and sparse ranges are represented compactly (a zeroed 1 GiB range is one ZERO segment, not a gibibyte of zeros) and applied correctly (punch hole / write zeros).
- **`target_size`** -- the file may grow or shrink between versions; apply truncates/extends to `target_size`.
- **`from_hash`** -- optional: the hash of the **full file content** this diff applies onto (the whole-file state before applying). It lets the **caller** verify a diff is applied to the intended baseline -- by comparing it to the baseline's known hash; the tool merely **carries** the field (exposed via `ReadHeader`) and **does not enforce it at apply time**, because the tool never computes hashes (doing so would require a full read of base, defeating reflink's read-only-changed-extents property). It stores the **file state**, not the hash of some diff object, so it is **invariant under compaction** (merge / roll-forward do not change the reconstructed state). Caller-supplied (content-addressed storage usually already has it), omitted by default, and not required for reconstruction.
- **`checksum`** -- the object's own integrity (detects corruption in transit or storage).

A diff object is a **single** self-contained blob: the trailer must be followed by end of stream (EOF), which lets a consumer reject trailing junk or a second concatenated object; each reader carries exactly one object. `Verify` reads in bounded memory; `Apply` (writing a file) patches per diff in a streaming fashion, while `ApplyTo`/`Reconstruct`/`Merge` buffer the participating diffs' data sections in memory (a diff is usually far smaller than base, but a full-change diff equals the whole file size).

The format deliberately stays close to a minimal, documented block-level diff (offset/length/data records with metadata) rather than a general delta codec.

## 5. The two modes and their compatibility

`reflink` compares the physical extents of `N` and `B` (via FIEMAP); diverging extents are candidates, and only the changed parts are read. `content` compares the full contents of the two files. The former requires a CoW filesystem and reflink lineage between `N` and `B`; the latter has no prerequisites.

The key point is that both **produce the same object format** and both guarantee `apply(B, D) == N`, so they are fully interchangeable:

- A `reflink`-mode diff covers a **superset** of the truly changed ranges (an extent moved physically but identical in content -- e.g. after defrag -- is also included; its bytes equal `B`'s, so applying it is a no-op). In the normal case (no movement between snapshots) it is in fact minimal.
- Because neither mode ever **misses** a change and both store the new bytes for every range they include, a chain may freely mix diffs from both modes; `apply` and `merge` treat them identically.

`reflink` is the fast path when its prerequisites hold; `content` is the always-correct fallback (lineage lost, non-CoW filesystem, a baseline materialized on another machine) and the ground truth for verification.

FIEMAP is only a **hint**: the implementation compares physical offsets to get candidate ranges, but for each candidate range it still reads the actual bytes from `N` into the diff, so correctness does not depend on physical-offset semantics that may vary across filesystems. An extent flagged `UNKNOWN`/`DELALLOC`/`ENCODED` (or inline/tail-packed) is treated as "changed" and read from `N` (conservative but correct).

## 6. reflink checkpoint -- why a related baseline is needed

The `reflink` fast path requires base and new to **share physical extents** -- so that "physically shared means unchanged" holds. A plain `cp` cannot give this: it produces an independent copy with its own blocks, so `FIEMAP` reports every extent as changed. A reflink clone (FICLONE / `cp --reflink=always`) is O(1) metadata sharing all extents; only ranges written afterward copy-on-write split and consume space. This is exactly what the `checkpoint` command does: perform FICLONE and verify the result really shares extents.

**Gotcha:** modern `cp` defaults to `--reflink=auto`, which on a non-reflink filesystem **silently** degrades to a full copy -- leaving you an independent file and a next diff quietly downgraded to a full scan. Always use `--reflink=always` or `cowdiff checkpoint` (the latter verifies sharing and errors if not shared).

Diffing between two **frozen** checkpoints (rather than against a live file) means the working file can keep changing while the diff is computed. A checkpoint is local; losing one (eviction, crash) is recoverable: materialize that state from the durable chain, do one `content`-mode diff, and re-establish a checkpoint.

## 7. Consistency

cowdiff operates on whatever bytes an input file currently holds; it does not freeze or lock. Presenting a consistent point is the caller's responsibility:

- **Crash-consistent**: `FICLONE` captures the file's extents at an instant -- equivalent to a power cut at that moment for whatever is writing it.
- **Clean-consistent** (no recovery needed afterward): briefly quiesce the writer (pause or flush) before `FICLONE`, then resume -- usually a sub-second stall.

Because diffs are taken between **frozen** checkpoints, the live file can keep changing throughout.

## 8. Correctness guarantees

- `apply(base, diff) == new`, byte for byte, in both modes.
- `apply(base, D1..Dk) == apply(base, merge(D1..Dk))`.
- apply is filesystem- and machine-independent (a diff is logical offset/length/bytes); the restoring side needs neither reflink nor a CoW filesystem.
- Objects carry a content checksum; the optional `from_hash` lets the **caller** verify apply lands on the right baseline (the tool carries it, does not enforce it; invariant under compaction).
- fork/rollback is just several diffs sharing a parent; supported at the object layer (the caller's manifest tracks reachability for GC).
- Sparse ranges, discard/trim, and file grow/shrink are represented and applied correctly (ZERO segments + `target_size`).

## 9. Related work and standards

cowdiff deliberately reuses well-understood building blocks rather than inventing new mechanisms:

- **FIEMAP / filefrag** (`e2fsprogs`) -- the authoritative way to read a file's physical extents; the basis of reflink mode.
- **FICLONE / reflink** -- checkpoint and reflink-accelerated apply.

## 10. Dependencies and portability

The only external module is `golang.org/x/sys/unix` (FICLONE, fallocate punch-hole, and the raw ioctl plumbing for FIEMAP). A few deliberate choices:

- **FIEMAP hand-rolled.** x/sys has no FIEMAP, so this project wraps the ioctl directly (~50 lines) rather than pulling in an unmaintained third-party library.
- **Checksum uses standard-library SHA-256.** `from_hash` is caller-supplied and never computed by the tool, so no extra hash library (such as BLAKE3) is needed.
- **Linux-only paths isolated by build tags.** reflink / FIEMAP / punch-hole carry `//go:build linux`; other platforms substitute stubs that return a "requires Linux" error. `content`-mode diff, apply, merge, and format encode/decode are portable and usable/testable on any platform.
