#!/usr/bin/env bash
# check-tap-formula.sh — compare the live Homebrew tap formula with brew/mgit.rb.
#
# hyper-swe/homebrew-tap's update-formula.yml rewrites ONLY the `version` line and
# the four `sha256` lines of Formula/mgit.rb, so every other line (install,
# caveats, depends_on) drifts silently whenever brew/mgit.rb changes and nobody
# copies it over. That is how the tap once installed both binaries but not the
# guest payload while CI stayed green (MGIT-65). This script is the detection.
#
#   bash scripts/ci/check-tap-formula.sh [--require] [--repo-formula FILE] [--tap-url URL]
#
# Exit 0  the bodies match (version and sha256 lines ignored), or the tap could not
#         be fetched: that prints "SKIPPED ... NOT CHECKED" and is NOT a pass.
# Exit 1  the bodies differ; the diff and the remedy are printed.
# Exit 3  the tap could not be fetched and --require was given (the pre-tag gate).
#
# Decision (MGIT-67): DETECT drift rather than generate the tap body from here.
# Generating would mean this repository pushing to the tap, which it cannot do
# and should not hold credentials for, and the tap's own workflow already owns
# the version and checksum lines. Detection costs one fetch, changes nothing
# anywhere, and a human still decides when to copy the body. Revisit if the
# tap's workflow is ever changed to take the body from this repository.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
REPO_FORMULA="$REPO/brew/mgit.rb"
TAP_URL="https://raw.githubusercontent.com/hyper-swe/homebrew-tap/main/Formula/mgit.rb"
REQUIRE=""
while [ "$#" -gt 0 ]; do
	case "$1" in
	--require) REQUIRE=1; shift ;;
	--repo-formula) REPO_FORMULA="${2:?}"; shift 2 ;;
	--tap-url) TAP_URL="${2:?}"; shift 2 ;;
	*) echo "check-tap-formula: unknown option '$1'" >&2; exit 2 ;;
	esac
done
[ -f "$REPO_FORMULA" ] || { echo "check-tap-formula: $REPO_FORMULA not found" >&2; exit 2; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/tapfm.XXXXXX")" || { echo "check-tap-formula: mktemp failed" >&2; exit 2; }
cleanup() { find "$WORK" -type f -delete; find "$WORK" -depth -type d -empty -delete; }
trap cleanup EXIT

skip() {
	echo "check-tap-formula: SKIPPED: $1" >&2
	echo "check-tap-formula: the tap formula was NOT CHECKED; this is not a pass" >&2
	[ -n "$REQUIRE" ] && exit 3
	exit 0
}

TAP="$WORK/tap.rb"
# The guard retries and bounds the fetch; a truncated file is removed first.
"$HERE/guard-fetch.sh" -t 60 -b 1 -n 2 -c "rm -f '$TAP'" -- curl -fsSL -o "$TAP" "$TAP_URL" >"$WORK/fetch.log" 2>&1 \
	|| skip "could not fetch $TAP_URL: $(tail -n 1 "$WORK/fetch.log")"
[ -s "$TAP" ] || skip "$TAP_URL returned an empty file"

# Only the lines the tap rewrites are ignored: `version "x"` and `sha256 "hex"`.
norm() { grep -Ev '^[[:space:]]*(version "[^"]*"|sha256 "[A-Fa-f0-9]*")[[:space:]]*$' "$1"; }
norm "$REPO_FORMULA" > "$WORK/repo.norm"
norm "$TAP" > "$WORK/tap.norm"

if diff -u "$WORK/tap.norm" "$WORK/repo.norm" > "$WORK/diff.txt"; then
	echo "check-tap-formula: the tap formula body matches brew/mgit.rb (version and sha256 lines ignored)"
	exit 0
fi
echo "check-tap-formula: DRIFT: the live tap formula body differs from brew/mgit.rb" >&2
echo "  (--- is the tap, +++ is this repository; version and sha256 lines are ignored)" >&2
sed 's/^/  /' "$WORK/diff.txt" >&2
echo "Remedy: copy the body of brew/mgit.rb into Formula/mgit.rb in hyper-swe/homebrew-tap," >&2
echo "keeping the tap's own version and sha256 lines, and commit it there. Do not touch the mtix formula." >&2
exit 1
