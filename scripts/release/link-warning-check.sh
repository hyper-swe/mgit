#!/usr/bin/env bash
# Fail if a linker log reports objects built for a newer macOS than the one
# being linked for. Apple's ld warns, and still succeeds, with:
#   ld: warning: object file (x.o) was built for newer 'macOS' version (15.0)
#   than being linked (14.0)
# which is the same defect as a wrong recorded minimum.
#
# Usage: link-warning-check.sh <link-log>
# Refs: HSWE-17.7.4
set -euo pipefail
log="${1:?usage: link-warning-check.sh <link-log>}"
[ -f "$log" ] || { echo "link-warning-check: no such log: $log" >&2; exit 2; }
# The text wraps in some tools, so match the two halves and not the whole line.
if grep -Eiq "built for (newer|a newer)" "$log" && grep -Eiq "than being linked" "$log"; then
	echo "link-warning-check: the linker reports objects built for a newer macOS than is being linked:" >&2
	grep -Ei "built for (newer|a newer)|than being linked" "$log" >&2
	exit 1
fi
echo "link-warning-check: no newer-macOS linker warning"
