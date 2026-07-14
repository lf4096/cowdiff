// Package cowdiff computes reflink-aware incremental binary diffs of large
// files on copy-on-write filesystems (XFS, Btrfs, bcachefs).
//
// See DESIGN.md for the object format, operations, and usage.
package cowdiff
