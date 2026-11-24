# FEAT-3.347.11 — the release stub-phrase check runs on every change

The release workflow refuses to ship when a non-test Go file holds one of its quoted stub phrases. That step ran only after a tag was pushed, so a phrase that reached main was found after the tag existed: the v0.8.0 run failed this way on three comments.

## Scenarios

1. `go test ./internal/packaging -run TestReleasePreflightAntiStubScanIsClean` reads the phrases from the anti-stub step in `.github/workflows/release.yml`, then scans every regular `.go` file outside `_test.go` from the repository root, as the step's `grep` does. A hit fails the test and lists each file, line and text.
2. The phrases come from the workflow, never from a copy in the test. A phrase added to the step is checked at once. If the step changes shape so that its pattern cannot be read, or the pattern gives fewer than three phrases, the test fails and says so. It never passes with nothing checked.
   grep reads the pattern as a basic regular expression, and the test compares literal text. A phrase that holds `.`, `[`, `]`, `*`, `^`, `$` or `\` could match differently in the two (`"work pending."` matches `"work pending!"` in grep), so the test refuses it and says why, rather than check it by a different rule.
3. At 744ffb8 the test fails on the three reviewlaunch comments. With their rewording (#394) it passes. Putting one comment back makes it fail again.
