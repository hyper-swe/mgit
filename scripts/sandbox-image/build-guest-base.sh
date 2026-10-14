#!/usr/bin/env bash
# build-guest-base.sh — assemble the glibc guest base: Debian slim, pinned by
# digest in pins.env, composed daemonlessly by `mgit sandbox base from`, plus
# the pinned verification tools unpacked under /opt/tools. Refs: FEAT-6.104
#
# Usage: build-guest-base.sh --print-plan <amd64|arm64>
#        build-guest-base.sh <amd64|arm64> <work-dir>
# --print-plan prints the plan and runs nothing. A build needs the disk and
# memory gates to pass first and runs one guest-free compose; the finished tree
# is measured against MAX_MIB and the build stops, trimming nothing, if over.
# The guest keeps no network and no credentials at run time.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=pins.env
. "$HERE/pins.env"

MAX_MIB=1229 # 1.2 GiB
print=0
if [ "${1:-}" = "--print-plan" ]; then print=1; shift; fi
ARCH="${1:?usage: build-guest-base.sh [--print-plan] <amd64|arm64> [work-dir]}"
case "$ARCH" in
	amd64) pin="$BUILDER_AMD64" ;;
	arm64) pin="$BUILDER_ARM64" ;;
	*) echo "build-guest-base: unknown arch '$ARCH'" >&2; exit 2 ;;
esac
digest="${pin#*@}"

if [ "$print" -eq 1 ]; then
	echo "base $digest"
	echo "compose mgit sandbox base from registry-1.docker.io/library/debian@$digest"
	echo "fetch guard-fetch.sh -t 900 -c none:every archive is digest-verified before use -n 3"
	bash "$HERE/fetch-guest-tools.sh" --print-urls "$ARCH" | while read -r tool sum url; do
		echo "tool $tool $sum $url"
	done
	echo "max-mib $MAX_MIB"
	echo "network none"
	exit 0
fi

WORK="${2:?usage: build-guest-base.sh <amd64|arm64> <work-dir>}"
echo "build-guest-base: the build stage needs a measured run under the resource gates; not implemented in this change" >&2
exit 3
