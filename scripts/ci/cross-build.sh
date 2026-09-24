#!/usr/bin/env bash
# cross-build.sh — compile every target the release ships, on every PR, with
# plain `go build`. Refs: MGIT-198
#
# The release assembler (.goreleaser.yaml) builds mgit for six platforms and
# the daemon and guest binaries for Linux, but CI built only on the runner's
# own platform, so a `syscall.Kill` in a file with no build tag broke the
# Windows targets of `mgit` on main for a day without a red anywhere — and the
# next release run would have failed at the build step, burning its tag. This
# script is the gate; TestCrossBuild_CoversEveryReleaseTarget pins its list to
# the assembler's so the two cannot drift.
#
# The two mgit-sandboxd builds the release ships are both CGO (libkrun) and
# are not here: the darwin one is built by the macOS libkrun job, and the
# Linux one (amd64 and arm64) by e2e.yml's linux-sandboxd job, through the
# same assembler the release runs (scripts/release/build-linux-sandboxd.sh).
# Everything else is CGO-free and cross-compiles from anywhere. The CGO-free
# Linux daemon (the firecracker build `go install` produces) is no longer a
# release target; the Linux Test job still compiles it on every run.
# Refs: MGIT-229
#
# Usage: bash scripts/ci/cross-build.sh
set -euo pipefail
cd "$(dirname "$0")/../.."

# TARGET  <goos>/<goarch>  <package>   — one line per release target.
TARGETS='
linux/amd64   ./cmd/mgit
linux/arm64   ./cmd/mgit
darwin/amd64  ./cmd/mgit
darwin/arm64  ./cmd/mgit
windows/amd64 ./cmd/mgit
windows/arm64 ./cmd/mgit
linux/amd64   ./cmd/mgit-guest
linux/arm64   ./cmd/mgit-guest
'
failed=0
while read -r target pkg; do
	[ -n "$target" ] || continue
	goos="${target%/*}"; goarch="${target#*/}"
	if CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -trimpath -o /dev/null "$pkg" 2>"/tmp/cross-build-$$.err"; then
		echo "  ok    $target  $pkg"
	else
		echo "  FAIL  $target  $pkg"
		sed 's/^/        /' "/tmp/cross-build-$$.err"
		failed=$((failed + 1))
	fi
done <<<"$TARGETS"
rm -f "/tmp/cross-build-$$.err"
if [ "$failed" -ne 0 ]; then
	echo "cross-build: $failed release target(s) do not compile — the release would fail at the build step" >&2
	exit 1
fi
echo "cross-build: every release target compiles"
