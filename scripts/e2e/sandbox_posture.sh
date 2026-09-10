#!/usr/bin/env bash
# Sandbox posture e2e (MGIT-48 job 3).
#
# With mgit-sandboxd present AND host virtualization available, runs the real
# containment path: launch a task sandbox and `mgit run -- echo ok` inside it,
# then a land round-trip. The GA backends (ADR-010) provision their guest
# differently, so the guest is provisioned from WHAT THE CALLER SUPPLIED:
#
#   kernel + rootfs  -> registered as an image (firecracker, vzf).
#   an OCI ref       -> composed into a directory base (libkrun, which needs no
#                       kernel of its own — libkrunfw supplies it).
#
# It used to branch on the OPERATING SYSTEM instead, which silently equated
# "Linux" with "firecracker". That was true while it was the only Linux
# backend and stopped being true when Linux libkrun was validated (MGIT-87):
# an entirely working Linux/libkrun daemon was sent down the firecracker branch
# and skipped for want of a kernel it does not use. The macOS half had the
# mirror-image bug until 2026-08-05 (MGIT-64/65 follow-up), where a fully
# working entitled Mac SKIPPED because kernel/rootfs vars were demanded
# unconditionally — so the mandatory macOS live release pass never exercised
# the shipped path. Dispatching on the inputs is what stops that recurring.
#
# This needs a KVM-capable Linux host or an entitled macOS arm64 host, so it
# GATES GRACEFULLY: when a prerequisite is missing it prints SKIP and exits 0
# (CI on hosted runners relies on this — see .github/workflows/e2e.yml's
# sandbox-posture job, which has no virtualization and expects the skip path
# itself to run clean). That tolerance is for CI ONLY. For a release-checklist
# run, SKIP is not an acceptable outcome for the platform you are checking:
# it means the live gate for that platform was NOT satisfied, full stop. Only
# a printed "SANDBOX POSTURE E2E: PASS (live)" counts.
#
# Usage: sandbox_posture.sh [bindir]
#   Guest inputs (supply the ONE your backend needs; on Linux, with none of
#   them, it skips — macOS defaults to the OCI form its GA backend uses):
#     MGIT_GUEST_IMAGE    a digest-pinned image ref ALREADY registered in the
#                         scratch repo's image set (rarely what you have), or
#     MGIT_GUEST_KERNEL + MGIT_GUEST_ROOTFS [+ MGIT_GUEST_CMDLINE]
#                         raw artifact paths for a kernel+rootfs backend;
#                         the script registers them inside its scratch repo
#                         (`sandbox image init` + `add`) and uses the resulting
#                         ref. This is the release-checklist form: image
#                         registration is PER-REPO (.mgit/sandbox), so a ref
#                         from another repo cannot resolve here. Or
#     MGIT_GUEST_OCI_REF  the OCI image to compose a directory guest base from,
#                         for the libkrun backend on either platform
#                         (macOS default: debian:12).
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
. "$here/lib.sh"

if [ "${1:-}" != "" ]; then export PATH="$1:$PATH"; fi
require_mgit

skip() {
	echo "SANDBOX POSTURE E2E: SKIP — $*"
	echo "  (a live per-platform pass is mandated by docs/release/RELEASE-CHECKLIST.md;"
	echo "   in CI on a hosted runner this is expected and fine, but for a release"
	echo "   checklist run on real hardware this means the gate is NOT satisfied)"
	exit 0
}

# --- Prerequisite gates -----------------------------------------------------
command -v mgit-sandboxd >/dev/null 2>&1 || skip "mgit-sandboxd not installed"

os="$(uname -s)"
case "$os" in
Linux)
	[ -e /dev/kvm ] || skip "no /dev/kvm (host lacks KVM / nested virt)"
	[ -r /dev/kvm ] && [ -w /dev/kvm ] || skip "/dev/kvm not accessible to this user"
	if [ -z "${MGIT_GUEST_IMAGE:-}" ] && [ -z "${MGIT_GUEST_OCI_REF:-}" ] &&
		{ [ -z "${MGIT_GUEST_KERNEL:-}" ] || [ -z "${MGIT_GUEST_ROOTFS:-}" ]; }; then
		skip "no guest input (set MGIT_GUEST_IMAGE, or MGIT_GUEST_KERNEL + MGIT_GUEST_ROOTFS for firecracker, or MGIT_GUEST_OCI_REF for a libkrun-linked daemon) — Linux has two backends and they take different guests"
	fi
	;;
Darwin)
	[ "$(uname -m)" = "arm64" ] || skip "macOS sandbox requires Apple Silicon (arm64)"
	# libkrun (the macOS GA backend, ADR-010) needs com.apple.security.hypervisor
	# specifically -- a DIFFERENT entitlement from vzf's com.apple.security.
	# virtualization. Checking the wrong one would pass a binary that cannot
	# actually drive the backend this platform ships.
	if ! codesign --display --entitlements - "$(command -v mgit-sandboxd)" 2>/dev/null |
		grep -q 'com.apple.security.hypervisor'; then
		skip "mgit-sandboxd lacks the com.apple.security.hypervisor entitlement (libkrun)"
	fi
	;;
*)
	skip "no sandbox backend on $os"
	;;
esac

# --- Live path --------------------------------------------------------------
work="$(mktemp -d)"
# Teardown removes what this run launched and then PROVES it: a daemon that
# outlives its mktemp root is exactly the leak MGIT-191 found six of. The stop
# is scoped to this root's daemon (by its recorded pid); the check is by root,
# so other repositories' daemons on this host are never touched or counted.
cleanup() {
	local status=$? leaked
	mgit sandbox daemons stop --repo-root "$work" >/dev/null 2>&1 || true
	# The check runs BEFORE the scratch is removed: with its root still present
	# the daemon cannot have drained itself, so what this proves is the stop —
	# not a race against the daemon's own self-drain. Keyed on --host-root,
	# which the CLI always passes.
	leaked="$(pgrep -f -- "--host-root $work/" 2>/dev/null || true)"
	rm -rf "$work"
	if [ -n "$leaked" ]; then
		echo "SANDBOX POSTURE E2E: FAIL -- daemon leaked after teardown (pid $leaked serving $work)" >&2
		exit 1
	fi
	echo "sandbox posture: no daemon serves $work after teardown"
	exit "$status"
}
trap cleanup EXIT
cd "$work"
git init -q
git -c user.email=e2e@mgit.local -c user.name=e2e commit -q --allow-empty -m init
mgit init >/dev/null

# Provision the guest from whichever input the caller supplied — NOT from the
# operating system's name (the image/base set is per-repo, so a ref from
# elsewhere cannot resolve here). On macOS the OCI form is the default because
# its GA backend takes no kernel of its own.
if [ -n "${MGIT_GUEST_IMAGE:-}" ]; then
	pass "using the pre-registered $MGIT_GUEST_IMAGE"
elif [ -n "${MGIT_GUEST_KERNEL:-}" ] && [ -n "${MGIT_GUEST_ROOTFS:-}" ]; then
	echo "== register guest image (kernel + rootfs) in the scratch repo =="
	mgit sandbox image init >/dev/null
	MGIT_GUEST_IMAGE="$(mgit sandbox image add --name base \
		--kernel "$MGIT_GUEST_KERNEL" --rootfs "$MGIT_GUEST_ROOTFS" \
		${MGIT_GUEST_CMDLINE:+--cmdline "$MGIT_GUEST_CMDLINE"} --json |
		sed -n 's/.*"image_ref":"\([^"]*\)".*/\1/p')"
	[ -n "$MGIT_GUEST_IMAGE" ] || _e2e_fail "image add produced no reference"
	pass "registered $MGIT_GUEST_IMAGE"
else
	oci_ref="${MGIT_GUEST_OCI_REF:-debian:12}"
	echo "== compose guest base from $oci_ref (libkrun, OCI) in the scratch repo =="
	# fetch-guard: `mgit sandbox base from` pulls an OCI image through the
	# product's own registry client (internal/sandboxd/guestbase/pull.go),
	# which already bounds a whole pull at 15 minutes -- clause 2, in Go. It
	# has no retry and no precondition restore, and wrapping the CLI here
	# would guard the wrong layer: a retry outside the client cannot clear the
	# half-written blob cache inside it. MGIT-145 carries that work.
	# Refs: MGIT-143, MGIT-145
	MGIT_GUEST_IMAGE="$(mgit sandbox base from "$oci_ref" --json |
		sed -n 's/.*"image_ref":"\([^"]*\)".*/\1/p')"
	[ -n "$MGIT_GUEST_IMAGE" ] || _e2e_fail "sandbox base from produced no reference"
	pass "composed $MGIT_GUEST_IMAGE from $oci_ref"
fi

echo "== launch a task sandbox and exec inside it =="
mgit work wt --task-id SB-1 --sandbox --image "$MGIT_GUEST_IMAGE" >/dev/null
# `set -e` + a command substitution is a diagnosability trap: a failing
# `mgit run` aborts the script with its output still inside the unassigned
# variable, so the log ends at the heading above and says NOTHING about why.
# That is exactly what a first Linux/libkrun run looked like (MGIT-87). Capture
# the status, then print what the command actually said before failing.
runout="$(cd wt && mgit run -- echo ok 2>&1)" && runrc=0 || runrc=$?
if [ "$runrc" -ne 0 ]; then
	echo "$runout"
	_e2e_fail "mgit run -- echo ok exited $runrc inside the sandbox (output above)"
fi
assert_contains "$runout" "ok" "mgit run -- echo ok executed inside the sandbox"

# `mgit doctor` inside the booted task worktree, asserted BY PROPERTY: each
# guest row's STATUS from --json, never a rendered sentence. The LIVE legs
# exercised launch/exec/sync/land and never asked doctor a thing, so a guest
# row that regressed on Linux was caught by nobody (MGIT-195). Expectations
# dispatch on the guest input like the launch above: a composed base is the
# libkrun form, where every guest row can run; a kernel+rootfs image is the
# firecracker form, which delivers the worktree as a launch-time image, so
# sync-verify and delivery have nothing to ask and must read not-checked —
# asserted, not skipped. Refs: MGIT-195, MGIT-159, MGIT-164, MGIT-174, MGIT-192
echo "== mgit doctor inside the task worktree, rows by status =="
doctor_status() { # $1 json, $2 row name -> the row's status, or MISSING
	printf '%s' "$1" | python3 -c '
import json, sys
name = sys.argv[1]
report = json.load(sys.stdin)
rows = report["checks"] if isinstance(report, dict) else report
print(next((r["status"] for r in rows if r.get("name") == name), "MISSING"))' "$2"
}
expect_row() { # $1 json, $2 row, $3 expected status
	got="$(doctor_status "$1" "$2")"
	[ "$got" = "$3" ] || _e2e_fail "doctor row $2: status $got, expected $3"
	pass "doctor row $2: $3"
}
docjson="$(cd wt && mgit doctor --json 2>/dev/null)" && docrc=0 || docrc=$?
[ -n "$docjson" ] || _e2e_fail "mgit doctor --json printed nothing (exit $docrc)"
# The backend is what the sandbox REPORTS, not what the script guessed from
# its inputs: the expectations below are the backend's, and a guess would be
# a second source of truth for the same fact.
backend="$(mgit sandbox status SB-1 --json | sed -n 's/.*"backend":"\([^"]*\)".*/\1/p')"
[ -n "$backend" ] || _e2e_fail "sandbox status --json names no backend"
pass "sandbox backend: $backend"
if [ "$backend" = "kvm" ]; then
	# firecracker, read live on 2026-09-10 (this gate's first reading of it):
	# the guest's name table is served; sync-verify reads FAILED, doctor's own
	# word for a guest with no sha256sum — the minimal busybox rootfs links
	# none — so nothing delivered into it can be confirmed from inside; that
	# is asserted as the documented state (the MGIT-87 pattern: link the
	# applet and this line turns red until it is updated), never skipped;
	# delivery is a launch-time image with nothing to ask, not-checked.
	expect_row "$docjson" daemon/loads ok
	expect_row "$docjson" guest/localhost ok
	expect_row "$docjson" guest/sync-verify failed
	expect_row "$docjson" guest/delivery not-checked
	[ "$docrc" -ne 0 ] || _e2e_fail "doctor exited 0 with a failed row"
else
	expect_row "$docjson" daemon/loads ok
	expect_row "$docjson" guest/localhost ok
	expect_row "$docjson" guest/sync-verify ok
	expect_row "$docjson" guest/delivery ok
	expect_row "$docjson" base/currency ok
	[ "$docrc" -eq 0 ] || _e2e_fail "doctor exited $docrc with every guest row ok"
	# The tamper: change one delivered byte in the daemon's staged copy of the
	# worktree on the host — the tree the guest reads — and doctor must say the
	# guest reads it differently, with exit 1; restore it and doctor recovers.
	sbid="$(mgit sandbox status SB-1 --json | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')"
	[ -n "$sbid" ] || _e2e_fail "sandbox status --json printed no id"
	# The daemon's runtime dir is XDG_RUNTIME_DIR or the temp dir, and the
	# per-sandbox state dir is named by the TAIL of the sandbox id.
	runtime_base="${XDG_RUNTIME_DIR:-${TMPDIR:-/tmp}}"
	staged="$(find "$runtime_base/mgit-$(id -u)" -type d -name worktree-staging -path "*${sbid: -8}*" 2>/dev/null | head -1)"
	[ -n "$staged" ] || _e2e_fail "no staged tree for sandbox $sbid under $runtime_base/mgit-$(id -u) (libkrun stages the worktree per VM)"
	victim="$staged/CLAUDE.md"
	[ -f "$victim" ] || _e2e_fail "the delivered tree has no CLAUDE.md to tamper with"
	cp "$victim" "$work/victim.orig"
	printf '\n# tampered on the host after delivery (MGIT-195)\n' >> "$victim"
	tampered="$(cd wt && mgit doctor --json 2>/dev/null)" && trc=0 || trc=$?
	expect_row "$tampered" guest/delivery failed
	[ "$trc" -ne 0 ] || _e2e_fail "doctor exited 0 with a delivered file tampered on the host"
	pass "doctor exited $trc on the tampered delivery"
	cp "$work/victim.orig" "$victim"
	restored="$(cd wt && mgit doctor --json 2>/dev/null)" || true
	expect_row "$restored" guest/delivery ok
fi

echo "== land round-trip =="
( cd wt
  printf 'contained\n' > built.txt
  mgit add . >/dev/null
  mgit commit -m 'work in sandbox' >/dev/null
)
# The land path verifies dual-hash + task binding + host-anchored attestation.
assert_ok "sandbox land succeeds" -- mgit sandbox land --task SB-1


# ---------------------------------------------------------------------------
# A guest that dies is a DEAD sandbox, not a running one (MGIT-99)
# ---------------------------------------------------------------------------
# Kill a second guest from inside — a tmpfs bigger than its memory, filled —
# then read what mgit says. Before MGIT-99: `running`, and every later command
# waited out a 15 s dial timeout to fail with the same advisory. The kill
# needs a mount, so it runs --as-root; the minimal firecracker rootfs links no
# mount, so this scenario is libkrun's (it is the backend a developer runs).
if [ "$backend" = "libkrun" ]; then
	echo "== a dead guest is reported dead and refused at once =="
	mgit work wt2 --task-id SB-2 --sandbox --image "$MGIT_GUEST_IMAGE" --memory-mb 512 >/dev/null
	assert_ok "the second guest answers" -- sh -c 'cd wt2 && mgit run -- /bin/echo alive'
	mgit sandbox exec --task SB-2 --as-root -- sh -c \
		'mkdir -p /mnt/t && mount -t tmpfs -o size=2g tmpfs /mnt/t && dd if=/dev/zero of=/mnt/t/x bs=1M count=1500' \
		>/dev/null 2>&1 || true
	state="$(mgit sandbox status SB-2 --json | sed -n 's/.*"state":"\([^"]*\)".*/\1/p')"
	[ "$state" = "dead" ] || _e2e_fail "a killed guest reads '$state', expected dead (MGIT-99)"
	pass "status says dead"
	t0=$(date +%s)
	refusal="$(cd wt2 && mgit run -- /bin/echo again 2>&1)" && _e2e_fail "an exec against a dead guest succeeded"
	elapsed=$(( $(date +%s) - t0 ))
	[ "$elapsed" -lt 5 ] || _e2e_fail "the refusal took ${elapsed}s — a dead guest must be refused before any dial, not after a 15 s timeout (MGIT-99)"
	assert_contains "$refusal" "mgit sandbox remove SB-2 --force" "the refusal names the remedy"
	assert_contains "$refusal" "--memory-mb 512" "the relaunch keeps the declared memory"
	pass "refused in ${elapsed}s with the remedy"
	t0=$(date +%s)
	(cd wt2 && mgit doctor --json >/dev/null 2>&1) || true
	elapsed=$(( $(date +%s) - t0 ))
	[ "$elapsed" -lt 10 ] || _e2e_fail "doctor took ${elapsed}s against a dead guest — its guest rows must not each wait out a dial timeout (MGIT-99)"
	pass "doctor answered in ${elapsed}s"
	assert_ok "remove tears the dead sandbox down" -- mgit sandbox remove SB-2 --force
fi

echo "SANDBOX POSTURE E2E: PASS (live)"
