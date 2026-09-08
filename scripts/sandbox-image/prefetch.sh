#!/usr/bin/env bash
# prefetch.sh — fetch one pinned, digest-verified file WITH RESUME, under the
# fetch guard's three clauses. Sourced, not run. Refs: MGIT-188, MGIT-143
#
# libkrunfw's own Makefile fetches its ~141 MB kernel tarball with a bare,
# single-shot curl. On 2026-08-23 a mirror closed the transfer at the same
# byte (68.0 M of 141 M) on all three guarded attempts of two jobs: the guard's
# restore clause could only delete the partial and start from zero, and a
# mirror that truncates at a fixed byte defeats that exactly. A RESUMED
# transfer asks for the bytes after the cut instead of the same first 68 MB
# again, and the digest check afterwards keeps "caching must not weaken
# verification" intact — the transfer's own opinion of success is never what
# decides.
#
#   prefetch_verified <url> <sha256> <dest> [attempt-timeout-seconds]
#
# On success <dest> exists with the pinned digest. A file already at <dest>
# with the pinned digest is left alone; one with another digest is refetched.
# On a digest mismatch after the transfer the file is DELETED and the function
# fails loudly — a wrong file must never sit where a later existence check
# would accept it (MGIT-143 clause 3). PREFETCH_NO_RESUME=1 disables resume
# (the self-test's negative control). Exercised by prefetch-selftest.sh
# against a server that truncates on purpose.

prefetch_sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}

prefetch_verified() {
	url="$1"; want="$2"; dest="$3"; bound="${4:-600}"
	# GUARD by that name, so the fetch inventory (scripts/ci/fetch-inventory.sh)
	# recognises the guarded site; a caller that already set GUARD keeps it.
	GUARD="${PREFETCH_GUARD:-${GUARD:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../ci/guard-fetch.sh}}"
	part="$dest.part"
	name="$(basename "$dest")"
	mkdir -p "$(dirname "$dest")"
	if [ -f "$dest" ]; then
		got="$(prefetch_sha256_of "$dest")"
		if [ "$got" = "$want" ]; then
			echo "prefetch: $name already present, digest verified"
			return 0
		fi
		echo "prefetch: $name is present but its digest is $got, not the pin; refetching"
		rm -f "$dest"
	fi
	resume="-C -"
	[ "${PREFETCH_NO_RESUME:-0}" = "1" ] && resume=""
	# The partial is the RESUME POINT, so the guard's restore clause keeps it:
	# every attempt asks for the bytes after the cut. The digest check below
	# is what rejects a short or corrupt file.
	# shellcheck disable=SC2086 # resume is two words on purpose
	if ! "$GUARD" -t "$bound" -l "prefetch-$name" \
		-c none:'the partial file is the resume point: the next attempt continues from its length, and the digest check after completion is what rejects a short or corrupt file' -- \
		curl -fL --retry 3 --retry-delay 2 --retry-all-errors $resume -o "$part" "$url"; then
		echo "prefetch: FATAL: $name could not be fetched from $url (the partial, if any, stays at $part for a later resume)" >&2
		return 1
	fi
	got="$(prefetch_sha256_of "$part")"
	if [ "$got" != "$want" ]; then
		echo "prefetch: FATAL: $name digest $got does not match the pin $want; the file is deleted so nothing can build from it" >&2
		rm -f "$part"
		return 1
	fi
	mv "$part" "$dest"
	echo "prefetch: $name fetched and verified ($want)"
}
