#!/usr/bin/env bash
# Sandbox posture e2e (MGIT-48 job 3).
#
# With mgit-sandboxd present AND host virtualization available, runs the real
# containment path: launch a task sandbox and `mgit run -- echo ok` inside it,
# then a land round-trip. This needs a KVM-capable Linux host or an entitled
# macOS arm64 host plus a provisioned guest image (MGIT-30), so it GATES
# GRACEFULLY: when a prerequisite is missing it prints SKIP and exits 0. The
# RELEASE checklist (docs/release/RELEASE-CHECKLIST.md) requires at least one
# live pass per platform, so the skip never hides an untested release.
#
# Usage: sandbox_posture.sh [bindir]
#   Env (either form provisions the live path; with neither, the job skips):
#     MGIT_GUEST_IMAGE    a digest-pinned image ref ALREADY registered in the
#                         scratch repo's image set (rarely what you have), or
#     MGIT_GUEST_KERNEL + MGIT_GUEST_ROOTFS [+ MGIT_GUEST_CMDLINE]
#                         raw artifact paths; the script registers them inside
#                         its scratch repo (`sandbox image init` + `add`) and
#                         uses the resulting ref. This is the release-checklist
#                         form: image registration is PER-REPO (.mgit/sandbox),
#                         so a ref from another repo cannot resolve here.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
. "$here/lib.sh"

if [ "${1:-}" != "" ]; then export PATH="$1:$PATH"; fi
require_mgit

skip() {
	echo "SANDBOX POSTURE E2E: SKIP — $*"
	echo "  (a live per-platform pass is mandated by docs/release/RELEASE-CHECKLIST.md)"
	exit 0
}

# --- Prerequisite gates -----------------------------------------------------
command -v mgit-sandboxd >/dev/null 2>&1 || skip "mgit-sandboxd not installed"

os="$(uname -s)"
case "$os" in
Linux)
	[ -e /dev/kvm ] || skip "no /dev/kvm (host lacks KVM / nested virt)"
	[ -r /dev/kvm ] && [ -w /dev/kvm ] || skip "/dev/kvm not accessible to this user"
	;;
Darwin)
	[ "$(uname -m)" = "arm64" ] || skip "macOS sandbox requires Apple Silicon (arm64)"
	# The daemon must be entitlement-signed to drive Virtualization.framework.
	if ! codesign --display --entitlements - "$(command -v mgit-sandboxd)" 2>/dev/null |
		grep -q 'com.apple.security.virtualization'; then
		skip "mgit-sandboxd lacks the com.apple.security.virtualization entitlement"
	fi
	;;
*)
	skip "no sandbox backend on $os"
	;;
esac

if [ -z "${MGIT_GUEST_IMAGE:-}" ] && { [ -z "${MGIT_GUEST_KERNEL:-}" ] || [ -z "${MGIT_GUEST_ROOTFS:-}" ]; }; then
	skip "no guest image (set MGIT_GUEST_IMAGE, or MGIT_GUEST_KERNEL + MGIT_GUEST_ROOTFS)"
fi

# --- Live path --------------------------------------------------------------
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
cd "$work"
git init -q
git -c user.email=e2e@mgit.local -c user.name=e2e commit -q --allow-empty -m init
mgit init >/dev/null

# Register the guest image inside THIS repo when raw artifacts were given
# (the image set is per-repo; refs do not carry across repos).
if [ -z "${MGIT_GUEST_IMAGE:-}" ]; then
	echo "== register guest image (kernel + rootfs) in the scratch repo =="
	mgit sandbox image init >/dev/null
	MGIT_GUEST_IMAGE="$(mgit sandbox image add --name base \
		--kernel "$MGIT_GUEST_KERNEL" --rootfs "$MGIT_GUEST_ROOTFS" \
		${MGIT_GUEST_CMDLINE:+--cmdline "$MGIT_GUEST_CMDLINE"} --json |
		sed -n 's/.*"image_ref":"\([^"]*\)".*/\1/p')"
	[ -n "$MGIT_GUEST_IMAGE" ] || _e2e_fail "image add produced no reference"
	pass "registered $MGIT_GUEST_IMAGE"
fi

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
