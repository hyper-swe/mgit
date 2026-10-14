#!/usr/bin/env bash
# verify-release.sh <keyring> <InRelease> <suite> <path> <file> <workdir> <name>
# Checks the InRelease signature with $GPGV (default gpgv) and requires a VALIDSIG status line
# (gpgv can exit 0 with only a warning), then reads ONLY the text between the signed-message header
# and the signature, after refusing any file that is not exactly one signed block. It refuses a
# Release for another suite and checks <file> against the hash the signed text lists for <path>.
# The keyring is the trust anchor: any key in it is accepted.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
GPGV="${GPGV:-gpgv}"
KEYRING="$1" RAW="$2" SUITE="$3" P="$4" FILE="$5" WORK="$6" NAME="$7"
"$GPGV" --status-fd 1 --keyring "$KEYRING" "$RAW" >"$WORK/$NAME.status" 2>"$WORK/$NAME.gpgv" \
	|| { echo "verify-release: the $SUITE Release signature did not verify: $(cat "$WORK/$NAME.gpgv")" >&2; exit 1; }
grep -q '^\[GNUPG:\] VALIDSIG ' "$WORK/$NAME.status" \
	|| { echo "verify-release: no valid signature was reported for the $SUITE Release: $(cat "$WORK/$NAME.gpgv")" >&2; exit 1; }
python3 "$HERE/debs-index.py" verify --inrelease "$RAW" --suite "$SUITE" --path "$P" --file "$FILE"
