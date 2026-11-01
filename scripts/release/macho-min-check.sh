#!/usr/bin/env bash
# Fail unless every Mach-O file given records a minimum macOS version that is
# present and no newer than <max>. A binary built on a newer runner image
# defaults its minimum to that image's macOS, and then refuses to start on the
# older macOS the release still supports.
#
# Usage: macho-min-check.sh <max-version> <file>...
# Refs: HSWE-17.7.4
set -euo pipefail

max="${1:?usage: macho-min-check.sh <max-version> <file>...}"
shift
[ "$#" -gt 0 ] || { echo "macho-min-check: no files given" >&2; exit 2; }

# newer A B: true when A is a later version than B.
newer() { [ "$1" != "$2" ] && [ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | tail -n 1)" = "$1" ]; }

bad=0
for f in "$@"; do
	mins="$(otool -l "$f" | awk '
		/cmd LC_BUILD_VERSION/ { want = "minos"; next }
		/cmd LC_VERSION_MIN_MACOSX/ { want = "version"; next }
		want != "" && $1 == want { print $2; want = "" }')"
	if [ -z "$mins" ]; then
		echo "macho-min-check: $f records no minimum macOS version" >&2
		bad=1
		continue
	fi
	for m in $mins; do
		if newer "$m" "$max"; then
			echo "macho-min-check: $f requires macOS $m, newer than the supported $max" >&2
			bad=1
		fi
	done
done
[ "$bad" = 0 ] || exit 1
echo "macho-min-check: every file's minimum is at most macOS $max"
