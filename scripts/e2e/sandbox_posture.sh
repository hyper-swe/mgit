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
trap 'rm -rf "$work"' EXIT
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

echo "== land round-trip =="
( cd wt
  printf 'contained\n' > built.txt
  mgit add . >/dev/null
  mgit commit -m 'work in sandbox' >/dev/null
)
# The land path verifies dual-hash + task binding + host-anchored attestation.
assert_ok "sandbox land succeeds" -- mgit sandbox land --task SB-1

echo "SANDBOX POSTURE E2E: PASS (live)"
