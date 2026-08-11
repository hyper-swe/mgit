#!/usr/bin/env bash
# The Linux/libkrun capability column, as MEASURED on real KVM (MGIT-87), in
# the form CI can enforce and a human can read.
#
# WHY A FILE RATHER THAN A LIST INSIDE THE WORKFLOW. The integrating lane runs
# its loop against a DECLARATION of what this backend can do on Linux, so that
# declaration has to be one artifact: the same names that gate CI are the names
# behind the tables in README.md and docs/INSTALL-SANDBOX.md. A list that lives
# only in YAML drifts from the prose the moment either is edited alone.
#
# THREE ASSERTIONS, and the second and third are the ones that make this a gate
# rather than a report:
#   1. every VALIDATED test PASSED — the column is still true;
#   2. every KNOWN_GAP test still FAILED — if one starts passing, the gap
#      closed and the declaration is now WRONG in the other direction. That is
#      a failure here on purpose: an unannounced capability is how a customer
#      ends up depending on something we never validated;
#   3. every TestE2E_Libkrun_RealVM_* in the package source is classified in one
#      of the two lists. A new test that nobody classified would otherwise be
#      silently ungated.
#
# Usage: libkrun_linux_column.sh <validated.log> <gap.log> [package-dir]
#        libkrun_linux_column.sh --validated-pattern | --gap-pattern
# Refs: MGIT-87, MGIT-78, ADR-010, ADR-011
set -uo pipefail

# --- The column, measured 2026-08-11 on Linux/KVM ---------------------------
# Boot, exec, the SEC-03 containment battery, host->guest sync of CONTENT, and
# artifact export all hold on Linux exactly as on macOS.
VALIDATED="
TestE2E_Libkrun_RealVM_Boots
TestE2E_Libkrun_RealVM_AgentCommitsInTheSandbox
TestE2E_Libkrun_RealVM_Litmus1_HostSSHsIntoTheGuest
TestE2E_Libkrun_RealVM_ConcurrentLaunches_AreIsolated
TestE2E_Libkrun_RealVM_NoneMode_NoEgress
TestE2E_Libkrun_RealVM_Allowlist_DefaultDeny
TestE2E_Libkrun_RealVM_Sync_HostEditReachesTheRunningGuest
TestE2E_Libkrun_RealVM_Sync_RefusesAConflictAndStillDeliversAfterward
TestE2E_Libkrun_RealVM_Sync_RefusesAHostAddOverAGuestCreatedFile
TestE2E_Libkrun_RealVM_Sync_StagedTreeNameMatchesTheBackend
TestE2E_Libkrun_RealVM_ArtifactExport_GuestBuiltTreeReachesTheHost
TestE2E_Libkrun_RealVM_ArtifactExport_HostileGuestIsRefused
TestE2E_Libkrun_RealVM_ModeFidelity_HostCanObserveTheModeTheGuestSet
TestE2E_Libkrun_RealVM_VirtiofsPerf
"

# Two measured gaps, both in libkrun's LINUX virtio-fs behaviour rather than in
# mgit's own logic, and both reproduced identically on a hosted runner and on
# bare metal:
#
#   (a) The guest's ROOT is effectively read-only: creating a file under the
#       writable-root overlay fails EOPNOTSUPP (/tmp and the worktree share are
#       fine). mgit-guest writes /etc/resolv.conf while configuring the
#       network, so it dies there and every allowlist/open sandbox exits before
#       it serves. That takes the live egress-policy verbs with it.
#   (b) `mgit run -- <bare command>` drops the guest exec channel; only an
#       absolute path works. That is a CLI/daemon-level gap rather than a test
#       in this package, so its tripwire is the shared posture script, run by
#       the workflow (see e2e.yml's posture tripwire step).
#   (c) A sync's CREATES and DELETES do not reach a running guest, although
#       content updates do. The verb reports the delete as applied and the
#       guest still reads the file — the silent-staleness failure MGIT-76's
#       e2e exists to catch.
#
# Only two representatives are RUN as the tripwire; the rest are listed so the
# classification below is complete. Refs: MGIT-87
KNOWN_GAP="
TestE2E_Libkrun_RealVM_Sync_RefusesADeleteOfAPathTheGuestChanged
TestE2E_Libkrun_RealVM_Allowlist_AllowedFlowSucceeds
TestE2E_Libkrun_RealVM_Allowlist_DNSResolvesAllowlistedName
TestE2E_Libkrun_RealVM_Allowlist_DenyIsAPolicyRefusal
TestE2E_Libkrun_RealVM_OpenMode_ReachesRawIP
TestE2E_Libkrun_RealVM_OpenMode_ResolvesThroughTheGateway
TestE2E_Libkrun_RealVM_LivePolicyRevoke
TestE2E_Libkrun_RealVM_Revoke_KillsEstablishedFlow
TestE2E_Libkrun_RealVM_Revoke_DrainKeepsEstablishedFlow
TestE2E_Libkrun_RealVM_ControlChannelIsNotVisibleToTheGuest
"

# INTERMITTENT on this backend, so gated NEITHER way: asserting a pass would
# make the gate flaky, and asserting a failure would entrench a defect that
# usually does not fire. The guest exec channel resets ("connection reset by
# peer") sometimes and not others — measured passing in three environments and
# then failing on a hosted runner with no code change (MGIT-91). They are RUN
# and their outcome is printed, so the flake rate stays visible; nothing hangs
# on the result. A capability that reaches this list is one the tables must not
# claim.
FLAKY="
TestE2E_Libkrun_RealVM_MgitGuestControlPlane
"

# Measurements, not capabilities: they need an input CI does not supply and are
# allowed to skip.
OPTIONAL="
TestE2E_Libkrun_RealVM_NpmTreePerf
"

# The subset the tripwire actually runs, one per gap mechanism.
GAP_TRIPWIRE="
TestE2E_Libkrun_RealVM_Sync_RefusesADeleteOfAPathTheGuestChanged
TestE2E_Libkrun_RealVM_Allowlist_AllowedFlowSucceeds
"

# runPattern prints a `go test -run` alternation for a list.
runPattern() { echo "^($(echo "$1" | tr -s '[:space:]' '|' | sed 's/^|//; s/|$//'))\$"; }

case "${1:-}" in
--validated-pattern)
	runPattern "$VALIDATED"
	exit 0
	;;
--gap-pattern)
	runPattern "$GAP_TRIPWIRE"
	exit 0
	;;
--flaky-pattern)
	runPattern "$FLAKY"
	exit 0
	;;
esac

validated_log="${1:?usage: libkrun_linux_column.sh <validated.log> <gap.log> [pkgdir]}"
gap_log="${2:?usage: libkrun_linux_column.sh <validated.log> <gap.log> [pkgdir]}"
pkgdir="${3:-internal/sandboxd/backend/libkrun}"

fail() {
	echo "FATAL: $*" >&2
	exit 1
}

# --- 1. every validated capability RAN and passed ---------------------------
for t in $VALIDATED; do
	grep -q -- "--- PASS: $t " "$validated_log" ||
		fail "$t did not PASS — a capability this repo DECLARES for Linux/libkrun is gone (or the test skipped)"
done
echo "validated column: $(echo "$VALIDATED" | grep -c .) capabilities passed"

# A skip inside the validated run means a prerequisite silently disappeared:
# with KVM, the library and MGIT_E2E_LIBKRUN=1 nothing in that set may skip.
if grep -- '--- SKIP' "$validated_log"; then
	fail "a validated Linux/libkrun e2e SKIPPED — the gate did not actually run"
fi

# --- 2. every known gap is STILL a gap --------------------------------------
for t in $GAP_TRIPWIRE; do
	if grep -q -- "--- PASS: $t " "$gap_log"; then
		fail "$t now PASSES on Linux/libkrun. That is good news and a gate failure:
  the measured gap it stands for has closed, so the capability tables in
  README.md, docs/INSTALL-SANDBOX.md and the CHANGELOG now understate what
  Linux can do. Move it from KNOWN_GAP to VALIDATED in this file and update
  those tables in the same commit."
	fi
	grep -q -- "--- FAIL: $t " "$gap_log" ||
		fail "$t neither passed nor failed in the tripwire run — it did not run at all"
done
echo "known gaps: $(echo "$GAP_TRIPWIRE" | grep -c .) tripwires still failing as documented"

# --- 3. nothing is unclassified ---------------------------------------------
missing=""
for t in $(grep -rho '^func \(TestE2E_Libkrun_RealVM_[A-Za-z0-9_]*\)' "$pkgdir" | sed 's/^func //'); do
	case " $(echo "$VALIDATED $KNOWN_GAP $FLAKY $OPTIONAL" | tr -s '[:space:]' ' ') " in
	*" $t "*) ;;
	*) missing="$missing $t" ;;
	esac
done
[ -z "$missing" ] || fail "unclassified real-VM test(s):$missing
  Every TestE2E_Libkrun_RealVM_* must be listed in this file as VALIDATED,
  KNOWN_GAP, FLAKY or OPTIONAL. An unlisted test is one nothing gates."
echo "classification: every real-VM test is accounted for"
echo "LINUX LIBKRUN COLUMN: ASSERTED"
