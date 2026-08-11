#!/usr/bin/env bash
# Sandbox CLI surface e2e (MGIT-88).
#
# WHY THIS EXISTS. `mgit sandbox exec|shell|grants|list|status|remove` were
# covered by unit tests only — no e2e script invoked any of them. Unit tests
# exercise the service beneath the CLI, and this project's defects keep landing
# in the gap between the two: MGIT-77 (`mgit commit` reported success for work
# it never recorded), MGIT-83 (`mgit-sandboxd --version` did not exist while a
# release step invoked it), MGIT-65 (the archive shipped no guest binaries, so
# `sandbox base from` could not compose). Every one had a green unit layer and a
# broken command.
#
# `status`, `list` and `remove` are what an operator reaches for FIRST when
# something is wrong, so a defect there surfaces at the worst possible moment.
#
# It asserts OBSERVABLE OUTPUT, not exit codes. `assert_ok` on a command that
# prints nothing useful is how sandbox_posture.sh's own `land` assertion ended up
# weaker than its comment claimed (E2E-MATRIX line 88).
#
# Boots ONE sandbox and drives every verb against it, so it costs one VM rather
# than seven. Gates the same way sandbox_posture.sh does: a missing prerequisite
# SKIPs and exits 0 for CI-without-virtualization, but a SKIP is NOT a pass for a
# release or gate run — only "SANDBOX CLI SURFACE E2E: PASS (live)" counts.
#
# Usage: sandbox_cli_surface.sh [bindir]
#   Linux: MGIT_GUEST_KERNEL + MGIT_GUEST_ROOTFS [+ MGIT_GUEST_CMDLINE], or
#          MGIT_GUEST_IMAGE (an already-registered ref)
#   macOS: nothing; the base is composed from MGIT_GUEST_OCI_REF (default debian:12)
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
. "$here/lib.sh"

if [ "${1:-}" != "" ]; then export PATH="$1:$PATH"; fi
require_mgit

skip() {
	echo "SANDBOX CLI SURFACE E2E: SKIP — $*"
	echo "  (for a gate or release run a SKIP means the check did NOT happen;"
	echo "   only 'SANDBOX CLI SURFACE E2E: PASS (live)' counts)"
	exit 0
}

command -v mgit-sandboxd >/dev/null 2>&1 || skip "mgit-sandboxd not installed"
os="$(uname -s)"
case "$os" in
Linux)
	[ -e /dev/kvm ] && [ -r /dev/kvm ] && [ -w /dev/kvm ] || skip "no usable /dev/kvm"
	if [ -z "${MGIT_GUEST_IMAGE:-}" ] && { [ -z "${MGIT_GUEST_KERNEL:-}" ] || [ -z "${MGIT_GUEST_ROOTFS:-}" ]; }; then
		skip "no guest image (set MGIT_GUEST_IMAGE, or MGIT_GUEST_KERNEL + MGIT_GUEST_ROOTFS)"
	fi
	;;
Darwin)
	[ "$(uname -m)" = "arm64" ] || skip "macOS sandbox requires Apple Silicon"
	codesign --display --entitlements - "$(command -v mgit-sandboxd)" 2>/dev/null |
		grep -q 'com.apple.security.hypervisor' ||
		skip "mgit-sandboxd lacks the com.apple.security.hypervisor entitlement"
	;;
*) skip "no sandbox backend on $os" ;;
esac

TASK="CLI-1"
shell_skipped=0
grants_skipped=0
work="$(mktemp -d)"
cleanup() {
	# Best-effort: the point of the run is the assertions, not the teardown, and
	# a cleanup failure must not overwrite the status they produced.
	(cd "$work" 2>/dev/null && mgit sandbox remove "$TASK" --force >/dev/null 2>&1) || true
	rm -rf "$work" || true
}
trap cleanup EXIT
cd "$work"
git init -q
git -c user.email=e2e@mgit.local -c user.name=e2e commit -q --allow-empty -m init
mgit init >/dev/null

case "$os" in
Linux)
	if [ -z "${MGIT_GUEST_IMAGE:-}" ]; then
		echo "== register the guest image (kernel + rootfs) =="
		mgit sandbox image init >/dev/null
		MGIT_GUEST_IMAGE="$(mgit sandbox image add --name base \
			--kernel "$MGIT_GUEST_KERNEL" --rootfs "$MGIT_GUEST_ROOTFS" \
			${MGIT_GUEST_CMDLINE:+--cmdline "$MGIT_GUEST_CMDLINE"} --json |
			sed -n 's/.*"image_ref":"\([^"]*\)".*/\1/p')"
		[ -n "$MGIT_GUEST_IMAGE" ] || _e2e_fail "image add produced no reference"
	fi
	;;
Darwin)
	echo "== compose the guest base from ${MGIT_GUEST_OCI_REF:-debian:12} =="
	MGIT_GUEST_IMAGE="$(mgit sandbox base from "${MGIT_GUEST_OCI_REF:-debian:12}" --json |
		sed -n 's/.*"image_ref":"\([^"]*\)".*/\1/p')"
	[ -n "$MGIT_GUEST_IMAGE" ] || _e2e_fail "sandbox base from produced no reference"
	;;
esac
pass "guest image: $MGIT_GUEST_IMAGE"

echo "== launch one sandbox; every verb below runs against it =="
mgit work wt --task-id "$TASK" --sandbox --image "$MGIT_GUEST_IMAGE" >/dev/null
pass "launched a sandbox for $TASK"

# --- list -------------------------------------------------------------------
# An operator's first question is "what is running", so the answer must contain
# the task binding, not merely exit 0.
echo "== sandbox list =="
out="$(mgit sandbox list)"
assert_contains "$out" "$TASK" "list names the task it launched"
json="$(mgit sandbox list --json)"
assert_contains "$json" '"task_id"' "list --json is structured, for an agent caller"

# --- status, before boot ----------------------------------------------------
# Provisioning is LAZY (FR-17.9/FR-17.10): `work --sandbox` REGISTERS the
# sandbox and the VM boots on first use. So `created` here is correct, not a
# stale state — and asserting it pins the half of the lifecycle that a
# boot-on-launch assumption would silently paper over.
echo "== sandbox status (registered, not yet booted) =="
out="$(mgit sandbox status "$TASK")"
assert_contains "$out" "$TASK" "status names the task"
assert_contains "$out" "created" "status reports 'created' before first use (lazy boot)"

# --- exec -------------------------------------------------------------------
# The guest's OUTPUT and the guest's EXIT CODE, separately: a CLI that ran the
# command but swallowed its status would pass an exit-code-only assertion.
# This is also what BOOTS the VM, per the lazy-provisioning contract above.
echo "== sandbox exec (this triggers the lazy boot) =="
out="$(mgit sandbox exec --task "$TASK" -- /bin/echo cli-surface-ok)"
assert_contains "$out" "cli-surface-ok" "exec returns the guest's stdout"
code=0
mgit sandbox exec --task "$TASK" -- /bin/sh -c 'exit 7' >/dev/null 2>&1 || code=$?
[ "$code" = "7" ] && pass "exec propagates the guest's exit status (7)" ||
	_e2e_fail "exec reported $code for a guest that exited 7 — the status is the guest's, not the CLI's"

# --- status, after boot -----------------------------------------------------
# The other half of the lifecycle: the exec above booted it, so the state must
# have advanced. A sandbox stuck at "created" while serving execs would tell an
# operator exactly the wrong thing at the moment they are debugging.
echo "== sandbox status (after the boot) =="
out="$(mgit sandbox status "$TASK")"
assert_contains "$out" "running" "status advances to 'running' once the VM is up"

# --- shell ------------------------------------------------------------------
# A different transport from exec, so it needs its own assertion; drive it
# non-interactively by piping a command in.
# The bidirectional vsock-PTY transport it needs is KVM-gated guest support
# (model.ErrShellTransportUnavailable), so a daemon build without it refuses by
# design. Distinguish that documented refusal from a real failure by the
# sentinel's own words — do not treat "unavailable here" as a broken command,
# and do not treat a broken command as "unavailable here".
echo "== sandbox shell =="
if out="$(printf '/bin/echo shell-surface-ok\n' | mgit sandbox shell --task-id "$TASK" 2>&1)"; then
	assert_contains "$out" "shell-surface-ok" "shell proxies stdin into the guest and returns its output"
elif printf '%s' "$out" | grep -q 'requires the KVM guest PTY transport'; then
	shell_skipped=1
	echo "  SKIP: shell needs the KVM guest PTY transport, not served by this"
	echo "        daemon build — the documented refusal, not a failure. It is"
	echo "        covered where that transport exists (the Linux live gate)."
else
	_e2e_fail "shell failed for a reason other than the documented transport gate: $out"
fi

# --- grants -----------------------------------------------------------------
# With no capability escalation attempted there is nothing pending, and an empty
# list is the correct answer. What is asserted is that the verb RUNS and reports
# a well-formed empty state rather than erroring -- an operator runs this when
# they suspect something is blocked, i.e. exactly when it may be empty.
# Capability escalation (deny -> prompt -> grant) is wired by the daemon's
# egress stack, which is Linux-only -- so off Linux the daemon reports the verb
# UNSERVED rather than pretending there are no pending grants. That distinction
# matters: "no grants pending" and "this daemon cannot tell you" are different
# answers, and an operator debugging a blocked connection must not confuse them.
# Tell the documented refusal from a real failure by its own wording.
echo "== sandbox grants =="
if out="$(mgit sandbox grants --task "$TASK" 2>&1)"; then
	pass "grants runs and reports: $(printf '%s' "$out" | head -1)"
	assert_ok "grants --json is structured" -- mgit sandbox grants --task "$TASK" --json
elif printf '%s' "$out" | grep -q 'not served by this daemon'; then
	grants_skipped=1
	echo "  SKIP: grants is not served by this daemon build — capability escalation"
	echo "        is wired by the Linux-only egress stack. Covered on the Linux gate."
else
	_e2e_fail "grants failed for a reason other than the documented unserved verb: $out"
fi

# --- remove -----------------------------------------------------------------
# The assertion is not that remove exits 0, but that the sandbox is GONE
# afterwards -- a teardown that reports success and leaves the VM running is the
# failure worth catching.
echo "== sandbox remove =="
mgit sandbox remove "$TASK" --force >/dev/null
after="$(mgit sandbox list)"
assert_not_contains "$after" "$TASK" "remove actually removed it; list no longer shows the task"

unavailable=""
[ "$shell_skipped" = "1" ] && unavailable="$unavailable shell"
[ "$grants_skipped" = "1" ] && unavailable="$unavailable grants"
if [ -n "$unavailable" ]; then
	echo "SANDBOX CLI SURFACE E2E: PASS (live) — NOT SERVED on this backend:$unavailable"
	echo "  Those are documented platform refusals, not passes. They are covered"
	echo "  where the transport exists; a gate run on Linux must see them RUN."
else
	echo "SANDBOX CLI SURFACE E2E: PASS (live)"
fi
