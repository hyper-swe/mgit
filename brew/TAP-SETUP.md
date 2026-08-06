# Homebrew tap wiring for mgit (status: live)

This describes how `hyper-swe/homebrew-tap` actually publishes the mgit
formula today — not a pending setup task. Verified by reading the tap's own
`Formula/mgit.rb` and `.github/workflows/update-formula.yml` directly (the
tap is public); do not assume from this repo's files alone.

## What is automatic

`.github/workflows/release.yml`'s `homebrew` job sends ONE
`repository_dispatch` (`event-type: release-published`) carrying only
`{"tag": "vX.Y.Z", "project": "mgit"}`. The tap's `update-formula.yml`
listens for that single event type — project-aware, shared with `mtix`, keyed
off `client_payload.project` — downloads that release's `checksums.txt`, and
in `Formula/<project>.rb`:
  - replaces `version "..."` with the tag's version, and
  - replaces the four `sha256 "..."` values (darwin arm64/amd64, linux
    arm64/amd64, in file order) with the matching checksums.

That is the ENTIRE automated surface. It never touches `install`, `caveats`,
`depends_on`, or anything else in the formula body.

## What is NOT automatic

Everything but version + the four checksums. If `brew/mgit.rb` in this repo
changes — the `install` method, `caveats`, `depends_on` — that change does
**not** ship until someone manually copies the updated body into
`Formula/mgit.rb` in `hyper-swe/homebrew-tap` and commits it there. This repo
cannot push to that repo directly; see docs/release/RELEASE-CHECKLIST.md for
when this manual sync is required.

## Install command (unchanged)

```bash
brew install hyper-swe/tap/mgit
```

Refs: MGIT-44
