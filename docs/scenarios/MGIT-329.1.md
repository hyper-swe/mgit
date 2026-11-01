# MGIT-329.1: the real-content size test in a shallow checkout

1. A contributor clones with `--depth 1` and runs `go test ./internal/store/git`.
2. Other tests leave ignored files in the tree (for example a built `mgit`).
3. Before: `TestAssertNotOversized_ThisRepositoryRealContent_DoesNotFire` failed with
   "unsupported git state: shallow clone", but passed when run alone on a clean tree.
4. After: the test sees the shallow marker first and skips with the reason, the same
   in every run order. CI checks out full history, so it still runs there.
