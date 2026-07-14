# Contributing

Issues and pull requests are welcome.

## Build and test

```sh
go build ./...
go vet ./...
gofmt -l .        # should print nothing
go test ./...
```

- The `checkpoint`, `diff --mode reflink`, and FIEMAP tests need **Linux + a CoW filesystem** (XFS reflink=1, Btrfs, bcachefs); they skip automatically elsewhere. To run them, point `TMPDIR` at a CoW filesystem and set `COWDIFF_REQUIRE_REFLINK=1` (fail instead of skip when reflink is unavailable).
- Random-test breadth is tunable: `COWDIFF_FUZZ_CHAINS=N` (content chains) and `COWDIFF_FUZZ_REFLINK=N` (reflink chains).

## Code style

- `gofmt` and `go vet` must be clean before submitting.
- Code and comments are in English; exported identifiers carry doc comments.
- Keep the dependency surface minimal (currently only `golang.org/x/sys`).

## Pull requests

- Keep a PR focused on one thing, with tests covering the change.
- Explain the motivation; if you change the diff object format, update [DESIGN.md](./DESIGN.md).
