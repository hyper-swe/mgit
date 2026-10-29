#!/usr/bin/env bash
# third-party-licenses.sh <out-file> — write the license texts of every Go
# module compiled into mgit's shipped binaries (mgit, mgit-sandboxd,
# mgit-guest) to one file, which the release archives carry beside NOTICE.
#
# The module list is what the Go toolchain says each binary links, for the
# platforms and the libkrun build tag the release builds. MGIT_LICENSES_MODULES_FILE
# replaces that list with a file of "path<TAB>version<TAB>dir" lines, so the
# refusal can be tested without a module cache.
#
# A module with no license file stops the build: shipping a binary without the
# license text of something compiled into it is the failure this prevents.
# The output is written whole or not at all.
# Refs: MGIT-306
set -euo pipefail

if [ "$#" -ne 1 ]; then
	echo "usage: third-party-licenses.sh <out-file>" >&2
	exit 2
fi
out="$1"
root="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$root"

work="$(mktemp -d "${TMPDIR:-/tmp}/third-party-licenses.XXXXXX")" || { echo "cannot create a scratch directory" >&2; exit 1; }
case "$work" in "${TMPDIR:-/tmp}"/third-party-licenses.*) ;; *) echo "refusing: unexpected scratch dir $work" >&2; exit 2 ;; esac
# The scratch directory is flat and this script's own: its three files, then it.
trap 'rm -f "$work/all.tsv" "$work/modules.tsv" "$work/body.txt"; rmdir "$work"' EXIT

modules="$work/modules.tsv"
if [ -n "${MGIT_LICENSES_MODULES_FILE:-}" ]; then
	sort -u "$MGIT_LICENSES_MODULES_FILE" >"$modules"
else
	: >"$work/all.tsv"
	for goos in linux darwin windows; do
		for tags in "" "libkrun"; do
			GOOS="$goos" go list -tags "$tags" -deps \
				-f '{{with .Module}}{{if not .Main}}{{.Path}}{{"\t"}}{{.Version}}{{"\t"}}{{.Dir}}{{end}}{{end}}' \
				./cmd/mgit ./cmd/mgit-sandboxd ./cmd/mgit-guest >>"$work/all.tsv"
		done
	done
	sort -u "$work/all.tsv" >"$modules"
fi
[ -s "$modules" ] || { echo "no modules found: nothing to write" >&2; exit 1; }

body="$work/body.txt"
{
	echo "Third-party licenses"
	echo
	echo "mgit's binaries include the Go modules below. Each module's license text"
	echo "follows, as published with that module."
	echo
} >"$body"

while IFS="$(printf '\t')" read -r path version dir; do
	[ -n "$path" ] || continue
	found=0
	for f in "$dir"/[Ll][Ii][Cc][Ee][Nn][SsCc][Ee]* "$dir"/[Cc][Oo][Pp][Yy][Ii][Nn][Gg]* "$dir"/[Nn][Oo][Tt][Ii][Cc][Ee]* "$dir"/[Pp][Aa][Tt][Ee][Nn][Tt][Ss]*; do
		[ -f "$f" ] || continue
		found=1
		{
			echo "== $path $version =="
			echo "-- $(basename "$f") --"
			cat "$f"
			echo
		} >>"$body"
	done
	if [ "$found" -eq 0 ]; then
		echo "refusing: $path $version has no license file in $dir" >&2
		exit 1
	fi
done <"$modules"

mkdir -p "$(dirname "$out")"
cp "$body" "$out"
echo "wrote $out ($(wc -l <"$out" | tr -d ' ') lines, $(wc -l <"$modules" | tr -d ' ') modules)"
