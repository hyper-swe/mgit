#!/usr/bin/env bash
# sandbox_posture_negative_control.sh [bindir] — proves sandbox_posture.sh's daemon-leak
# check CAN fail (MGIT-191). It derives a copy of the harness AT RUN TIME with the scoped
# stop removed (so the copy cannot drift from the harness), runs it beside the shared
# library, and requires the run to FAIL on the leak line. A harness whose negative
# control passes is a phantom gate — the first version of this check was exactly that.
#
# Live only: it needs whatever sandbox_posture.sh needs. Refs: MGIT-191, R-H300
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
copy="$here/.sandbox_posture_negative_control.generated.sh"
trap 'rm -f "$copy"' EXIT
sed 's|^\tmgit sandbox daemons stop --repo-root "\$work" >/dev/null 2>\&1 \|\| true$|\t: # NEGATIVE CONTROL: the scoped stop is removed|' \
	"$here/sandbox_posture.sh" > "$copy"
if ! grep -q "NEGATIVE CONTROL: the scoped stop is removed" "$copy"; then
	echo "NEGATIVE CONTROL: could not remove the stop line from sandbox_posture.sh — the harness changed shape; update this control" >&2
	exit 2
fi
out="$(bash "$copy" "${1:-}" 2>&1)" && rc=0 || rc=$?
if [ "$rc" -eq 1 ] && printf '%s\n' "$out" | grep -q "daemon leaked after teardown"; then
	echo "NEGATIVE CONTROL: PASS — the leak check fired with the stop removed (exit 1: $(printf '%s\n' "$out" | grep 'daemon leaked' | head -1 | cut -c1-120))"
	exit 0
fi
if printf '%s\n' "$out" | grep -q "SKIP"; then
	echo "NEGATIVE CONTROL: SKIP — the harness itself skipped on this host"
	exit 0
fi
echo "NEGATIVE CONTROL: FAIL — the leak check did NOT fire with the stop removed (exit $rc). The harness's teardown check is not proving anything:" >&2
printf '%s\n' "$out" | tail -6 >&2
exit 1
