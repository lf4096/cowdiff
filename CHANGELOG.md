# Changelog

## [0.1.0] - 2026-07-17

Initial release.

- reflink-aware incremental binary diff of large files on CoW filesystems.
- CLI: `checkpoint`, `diff`, `apply`, `merge`, `info`, `verify`.
- Two diff modes -- `reflink` (FIEMAP) and `content` -- sharing one object format.
- Go API: `Diff`, `Apply`, `ApplyTo`, `Reconstruct`, `Merge`, `Checkpoint`, `ReadHeader`, `Verify`.
- Atomic writes, input/output alias guards, bounded-memory `Verify`.
- Single external dependency (`golang.org/x/sys`); Linux-only paths behind build tags.

[0.1.0]: https://github.com/lf4096/cowdiff/releases/tag/v0.1.0
