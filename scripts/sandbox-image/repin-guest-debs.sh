#!/usr/bin/env bash
# repin-guest-debs.sh — re-pin the guest .deb lock against a bookworm snapshot AND
# a bookworm-security snapshot. Refs: FEAT-6.104.1
#
#   repin-guest-debs.sh --arch <amd64|arm64> --main-snapshot <TS> --security-snapshot <TS> \
#       --base-status <dpkg status of the base image> --keyring <debian-archive-keyring.gpg> \
#       --out-dir <dir> [--base-url https://snapshot.debian.org] [--want "git make"]
#
# Each Release file's signature is verified with gpgv (the command is $GPGV, default
# gpgv) against the keyring, and each Packages.xz against the hash the signed Release
# lists, BEFORE anything is read from it. A missing gpgv, a bad signature or a hash
# mismatch fails closed and writes nothing. The lock and the base-requirements file are
# written to <out-dir>; nothing is downloaded but the two Release files and two indexes.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
GPGV="${GPGV:-gpgv}"
BASE_URL="https://snapshot.debian.org"
WANT="git make"
ARCH="" MTS="" STS="" STATUS="" KEYRING="" OUT=""
while [ $# -gt 0 ]; do
	case "$1" in
		--arch) ARCH="$2"; shift 2 ;;
		--main-snapshot) MTS="$2"; shift 2 ;;
		--security-snapshot) STS="$2"; shift 2 ;;
		--base-status) STATUS="$2"; shift 2 ;;
		--keyring) KEYRING="$2"; shift 2 ;;
		--out-dir) OUT="$2"; shift 2 ;;
		--base-url) BASE_URL="$2"; shift 2 ;;
		--want) WANT="$2"; shift 2 ;;
		*) echo "repin-guest-debs: unknown argument '$1'" >&2; exit 2 ;;
	esac
done
for v in ARCH MTS STS STATUS KEYRING OUT; do
	[ -n "${!v}" ] || { echo "repin-guest-debs: missing --$(echo "$v" | tr 'A-Z' 'a-z')" >&2; exit 2; }
done
case "$ARCH" in amd64|arm64) ;; *) echo "repin-guest-debs: unknown arch '$ARCH'" >&2; exit 2 ;; esac

if ! command -v "$GPGV" >/dev/null 2>&1; then
	echo "repin-guest-debs: NOT CHECKED: '$GPGV' is not installed, so no Release signature can be verified; nothing was fetched or written" >&2
	exit 3
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/repin.XXXXXX")" || { echo "repin-guest-debs: mktemp failed" >&2; exit 1; }
cleanup() { find "$WORK" -type f -delete; find "$WORK" -depth -type d -empty -delete; }
trap cleanup EXIT

fetch_index() { # fetch_index <name> <archive dir> <suite> <snapshot>
	local name="$1" dir="$2" suite="$3" ts="$4"
	local url="$BASE_URL/archive/$dir/$ts/dists/$suite"
	curl -fsSL -o "$WORK/$name.InRelease" "$url/InRelease"
	curl -fsSL -o "$WORK/$name.xz" "$url/main/binary-$ARCH/Packages.xz"
	GPGV="$GPGV" bash "$HERE/verify-release.sh" "$KEYRING" "$WORK/$name.InRelease" "$suite" "main/binary-$ARCH/Packages.xz" "$WORK/$name.xz" "$WORK" "$name" || exit 1
	xz -dc "$WORK/$name.xz" > "$WORK/$name.idx"
}
fetch_index main debian bookworm "$MTS"
fetch_index security debian-security bookworm-security "$STS"

python3 "$HERE/debs-index.py" merge --main "$WORK/main.idx" --security "$WORK/security.idx" --out "$WORK/merged.idx"
# word splitting of $WANT is intended: a list of package names
# shellcheck disable=SC2086
python3 "$HERE/debs-closure.py" --requires-out "$WORK/requires" "$STATUS" "$WORK/merged.idx" $WANT > "$WORK/closure.jsonl"

mkdir -p "$OUT"
python3 "$HERE/debs-index.py" lock --jsonl "$WORK/closure.jsonl" --base-url "$BASE_URL" \
	--main-snapshot "$MTS" --security-snapshot "$STS" --arch "$ARCH" --out "$OUT/guest-debs-$ARCH.lock"
cp "$WORK/requires" "$OUT/guest-debs-$ARCH.requires"
echo "repin-guest-debs: wrote $OUT/guest-debs-$ARCH.lock ($(grep -vc '^#' "$OUT/guest-debs-$ARCH.lock") packages) and .requires"
