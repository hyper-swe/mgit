#!/usr/bin/env bash
# Assemble the publishable guest-image bundle: build every platform's bundle
# into ONE directory (a combined manifest.json covering all platforms + the
# per-platform kernel/rootfs artifacts) and write a checksums.txt. The result
# is what gets uploaded as release assets and what `mgit sandbox image install`
# consumes (from a local dir or the release's asset base URL). Refs: MGIT-61.2
#
# Usage: publish.sh <out-dir> [platform ...]
#   default platforms: darwin/arm64 linux/amd64 linux/arm64
#
# The actual upload/release cut is owner-triggered (attach the contents of
# <out-dir> to a GitHub release; `mgit sandbox image install` with no --from
# defaults to the latest release's asset base).
set -euo pipefail

DIR="${1:?usage: publish.sh <out-dir> [platform ...]}"; shift || true
HERE="$(cd "$(dirname "$0")" && pwd)"
mkdir -p "$DIR"; DIR="$(cd "$DIR" && pwd)"
platforms=("$@")
if [ "${#platforms[@]}" -eq 0 ]; then
	platforms=(darwin/arm64 linux/amd64 linux/arm64)
fi

for p in "${platforms[@]}"; do
	echo "########## building bundle: $p ##########"
	bash "$HERE/build-bundle.sh" "$p" "$DIR"
done

echo "########## checksums ##########"
( cd "$DIR" && : > checksums.txt
  for f in manifest.json vmlinux-* rootfs-*; do
	[ -e "$f" ] || continue
	printf '%s  %s\n' "$(shasum -a 256 "$f" | cut -d' ' -f1)" "$f" >> checksums.txt
  done
  cat checksums.txt )

echo "########## publishable layout ready: $DIR ##########"
echo "Attach every file in $DIR to a GitHub release; install then works via"
echo "  mgit sandbox image install            # defaults to the latest release assets"
echo "  mgit sandbox image install --from $DIR # or this local directory"
