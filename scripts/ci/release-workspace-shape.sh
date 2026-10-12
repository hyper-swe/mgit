#!/usr/bin/env bash
# release-workspace-shape.sh — make this checkout look like the release job's,
# so CI can run the whole suite where a release runs it.
#
# The release job's checkout is not main's: release.yml downloads the daemons
# into dist-prebuilt/ and the corresponding sources into dist-sources/, and
# GoReleaser writes dist/. A test that walks the repository's working tree met
# a path there that no CI checkout has, and the v0.7.0 release failed in its
# before-hook with the suite green on main. Refs: MGIT-281, MGIT-274
#
# Usage: release-workspace-shape.sh        (CI only; see the guard)
#
# The directory names are pinned to release.yml by
# internal/packaging/release_workspace_test.go, so they cannot drift.
set -euo pipefail

# Fail closed: this writes into the checkout it runs in. It runs only where
# the checkout is disposable (a CI job, or an explicit opt-in), and it refuses
# a directory that already exists rather than writing over someone's build.
if [ "${GITHUB_ACTIONS:-}" != "true" ] && [ "${MGIT_RELEASE_SHAPE:-}" != "1" ]; then
  echo "release-workspace-shape: refusing to write into a checkout outside CI (set MGIT_RELEASE_SHAPE=1 to opt in)" >&2
  exit 2
fi

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
[ -f "$root/go.mod" ] || { echo "release-workspace-shape: $root is not the module root" >&2; exit 2; }
cd "$root"

for d in dist-prebuilt dist-sources dist; do
  if [ -e "$d" ]; then
    echo "release-workspace-shape: $d/ already exists; refusing to write over it" >&2
    exit 2
  fi
done

# The daemons, as the artifact downloads land them (merge-multiple into one
# directory): a binary per platform and the bundled libraries beside it.
mkdir -p dist-prebuilt/linux_amd64/lib dist-prebuilt/linux_arm64/lib dist-prebuilt/darwin_arm64/lib
for p in linux_amd64 linux_arm64 darwin_arm64; do
  printf 'stand-in daemon for %s\n' "$p" > "dist-prebuilt/$p/mgit-sandboxd"
  chmod +x "dist-prebuilt/$p/mgit-sandboxd"
  printf 'stand-in library for %s\n' "$p" > "dist-prebuilt/$p/lib/libkrun.1"
done

# The corresponding sources, which the release attaches.
mkdir -p dist-sources
printf 'stand-in source archive\n' > dist-sources/libkrun-source.tar.gz
printf 'stand-in source archive\n' > dist-sources/libkrunfw-source.tar.gz

# GoReleaser's own output directory.
mkdir -p dist/mgit_linux_amd64
printf 'stand-in build output\n' > dist/mgit_linux_amd64/mgit
printf 'stand-in checksums\n' > dist/checksums.txt

echo "release-workspace-shape: dist-prebuilt/, dist-sources/ and dist/ created in $root"
