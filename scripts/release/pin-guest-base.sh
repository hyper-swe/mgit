#!/usr/bin/env bash
# pin-guest-base.sh — refresh the guest base record a release vouches for.
#
# A release RECORDS the base image digest it is smoke-tested with, in
# internal/sandboxd/guestbase/release-base.json (embedded in the binary):
# `mgit sandbox base from` with no reference composes it, the e2e smoke
# composes it, and `mgit doctor` states a base composed from anything else
# as a difference. This script is the one way the record changes: it asks
# the registry what the image's tag points at NOW and writes both halves —
# the tag for a reader, the digest for the pull. Run it before a cut; the
# release preflight (scripts/ci/release-preflight.sh, check 7) refuses a
# record that is missing or whose digest the registry no longer serves.
# Refs: MGIT-219, MGIT-218, MGIT-147
#
# Usage: scripts/release/pin-guest-base.sh [<image:tag>]
#   default image: the record's current image, or debian:12 when there is none
#   MGIT=<path> picks the mgit binary (default: mgit on PATH)
# fetch-guard: the only network call is `mgit sandbox base resolve`, one
# manifest request through the product's own registry client.
set -euo pipefail
MGIT="${MGIT:-mgit}"
root="$(git rev-parse --show-toplevel)"
rec="$root/internal/sandboxd/guestbase/release-base.json"

image="${1:-}"
if [ -z "$image" ] && [ -f "$rec" ]; then
	image="$(sed -n 's/.*"image": *"\([^"]*\)".*/\1/p' "$rec" | head -1)"
fi
[ -n "$image" ] || image=debian:12
case "$image" in *@sha256:*) echo "pin-guest-base: give the image by TAG; the digest is what this script resolves" >&2; exit 2 ;; esac

old=""
[ -f "$rec" ] && old="$(grep -o 'sha256:[0-9a-f]\{64\}' "$rec" | head -1 || true)"

command -v "$MGIT" >/dev/null 2>&1 || { echo "pin-guest-base: $MGIT is not available (cannot resolve)" >&2; exit 1; }
out="$("$MGIT" sandbox base resolve "$image" --json)" || { echo "pin-guest-base: resolve failed for $image" >&2; exit 1; }
new="$(sed -n 's/.*"digest":"\(sha256:[0-9a-f]\{64\}\)".*/\1/p' <<<"$out" | head -1)"
[ -n "$new" ] || { echo "pin-guest-base: $MGIT resolved no digest for $image: $out" >&2; exit 1; }

printf '{\n  "image": "%s",\n  "digest": "%s"\n}\n' "$image" "$new" >"$rec"
echo "$image → $new"
if [ -z "$old" ]; then echo "record written: $rec"
elif [ "$old" = "$new" ]; then echo "record unchanged"
else echo "record moved: was $old"; fi
# The record is EMBEDDED at build time: a binary built before this run still
# carries the previous one. Say so, so the next `mgit sandbox base from` is
# not composed from a stale record by a stale binary.
[ "$old" = "$new" ] || echo "rebuild mgit to embed the new record (go build -o build/mgit ./cmd/mgit/); the digest above is the image index — the same on every host — so a pin made here composes each host's own platform"
