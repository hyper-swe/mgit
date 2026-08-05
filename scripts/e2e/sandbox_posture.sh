#!/usr/bin/env bash
# Sandbox posture e2e (MGIT-48 job 3).
#
# With mgit-sandboxd present AND host virtualization available, runs the real
# containment path: launch a task sandbox and `mgit run -- echo ok` inside it,
# then a land round-trip. The two GA backends (ADR-010) provision their guest
# differently, so this branches by platform rather than sharing one flow:
#
#   Linux (firecracker) needs a kernel + rootfs image, same as before.
#   macOS (libkrun) needs NEITHER — libkrunfw supplies the kernel, and the
#     guest base is composed from an OCI image (`mgit sandbox base from`).
#     An earlier version of this script required MGIT_GUEST_IMAGE/KERNEL/
#     ROOTFS unconditionally, which do not apply to libkrun at all — so on a
#     fully working, entitlement-signed macOS host it SKIPPED regardless,
#     and the mandatory macOS live release pass (docs/release/
#     RELEASE-CHECKLIST.md) never actually exercised the shipped path.
#     Fixed 2026-08-05 (MGIT-64/65 follow-up).
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
#   Linux env (either form provisions the live path; with neither, it skips):
#     MGIT_GUEST_IMAGE    a digest-pinned image ref ALREADY registered in the
#                         scratch repo's image set (rarely what you have), or
#     MGIT_GUEST_KERNEL + MGIT_GUEST_ROOTFS [+ MGIT_GUEST_CMDLINE]
#                         raw artifact paths; the script registers them inside
#                         its scratch repo (`sandbox image init` + `add`) and
#                         uses the resulting ref. This is the release-checklist
#                         form: image registration is PER-REPO (.mgit/sandbox),
#                         so a ref from another repo cannot resolve here.
#   macOS env (optional):
#     MGIT_GUEST_OCI_REF  the OCI image to compose the guest base from
#                         (default: debian:12). No kernel/rootfs vars needed.
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
	if [ -z "${MGIT_GUEST_IMAGE:-}" ] && { [ -z "${MGIT_GUEST_KERNEL:-}" ] || [ -z "${MGIT_GUEST_ROOTFS:-}" ]; }; then
		skip "no guest image (set MGIT_GUEST_IMAGE, or MGIT_GUEST_KERNEL + MGIT_GUEST_ROOTFS) — firecracker (the Linux GA backend) needs a kernel+rootfs, it does not compose from OCI"
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

# Provision the guest the way THIS platform's backend actually needs (the
# image/base set is per-repo; a ref from elsewhere cannot resolve here).
case "$os" in
Linux)
	if [ -z "${MGIT_GUEST_IMAGE:-}" ]; then
		echo "== register guest image (kernel + rootfs, firecracker) in the scratch repo =="
		mgit sandbox image init >/dev/null
		MGIT_GUEST_IMAGE="$(mgit sandbox image add --name base \
			--kernel "$MGIT_GUEST_KERNEL" --rootfs "$MGIT_GUEST_ROOTFS" \
			${MGIT_GUEST_CMDLINE:+--cmdline "$MGIT_GUEST_CMDLINE"} --json |
			sed -n 's/.*"image_ref":"\([^"]*\)".*/\1/p')"
		[ -n "$MGIT_GUEST_IMAGE" ] || _e2e_fail "image add produced no reference"
		pass "registered $MGIT_GUEST_IMAGE"
	fi
	;;
Darwin)
	oci_ref="${MGIT_GUEST_OCI_REF:-debian:12}"
	echo "== compose guest base from $oci_ref (libkrun, OCI) in the scratch repo =="
	MGIT_GUEST_IMAGE="$(mgit sandbox base from "$oci_ref" --json |
		sed -n 's/.*"image_ref":"\([^"]*\)".*/\1/p')"
	[ -n "$MGIT_GUEST_IMAGE" ] || _e2e_fail "sandbox base from produced no reference"
	pass "composed $MGIT_GUEST_IMAGE from $oci_ref"
	;;
esac

echo "== launch a task sandbox and exec inside it =="
mgit work wt --task-id SB-1 --sandbox --image "$MGIT_GUEST_IMAGE" >/dev/null
runout="$(cd wt && mgit run -- echo ok 2>&1)"
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
