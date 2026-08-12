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
TestE2E_Libkrun_RealVM_MgitGuestControlPlane
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
TestE2E_Libkrun_RealVM_Allowlist_AllowedFlowSucceeds
TestE2E_Libkrun_RealVM_Allowlist_DNSResolvesAllowlistedName
TestE2E_Libkrun_RealVM_Allowlist_DenyIsAPolicyRefusal
TestE2E_Libkrun_RealVM_OpenMode_ReachesRawIP
TestE2E_Libkrun_RealVM_OpenMode_ResolvesThroughTheGateway
TestE2E_Libkrun_RealVM_LivePolicyRevoke
TestE2E_Libkrun_RealVM_Revoke_KillsEstablishedFlow
TestE2E_Libkrun_RealVM_Revoke_DrainKeepsEstablishedFlow
TestE2E_Libkrun_RealVM_ControlChannelIsNotVisibleToTheGuest
TestE2E_Libkrun_RealVM_Sync_RefusesADeleteOfAPathTheGuestChanged
"

# NO known gaps remain in this package. The list is kept — not deleted — so
# that the next one has an obvious home and the tripwire machinery below stays
# exercised in review rather than being rebuilt from memory.
#
# What was here: the networked half went VALIDATED when MGIT-89 was fixed (the
# guest could not write /etc, so mgit-guest died configuring its resolver), and
# the sync-delete case went VALIDATED when MGIT-90 was fixed (a deleted path
# kept serving its old CONTENT to a guest whose kernel had cached the name).
# Refs: MGIT-87, MGIT-89, MGIT-90
KNOWN_GAP=""

# INTERMITTENT tests are gated NEITHER way: asserting a pass would make the
# gate flaky, and asserting a failure would entrench a defect that usually does
# not fire. The list is EMPTY and kept so the next one has an obvious home.
#
# What was here: MgitGuestControlPlane, which reset roughly one run in ten
# until MGIT-91. Two causes, one product and one test — the manager's
# first-command retry never fired for a connection RESET (it matched only
# io.EOF, and libkrun's host-side vsock socket exists before the guest binds,
# so a too-early exec connects and is reset), and this particular test waited
# on a console marker that mgit-guest prints BEFORE it binds its listener. It
# passed 20 consecutive runs after both were fixed, which is what justified
# moving it into VALIDATED rather than one green run. Refs: MGIT-91
FLAKY=""

# Measurements, not capabilities: they need an input CI does not supply and are
# allowed to skip.
OPTIONAL="
TestE2E_Libkrun_RealVM_NpmTreePerf
"

# The subset the tripwire actually runs, one per gap mechanism.
GAP_TRIPWIRE=""

# runPattern prints a `go test -run` alternation for a list.
# runPattern prints a `go test -run` alternation for a list, or NOTHING for an
# empty one — the caller must treat empty as "there is nothing to run" rather
# than passing `^()$`, which matches every test name and would turn an empty
# gap list into a full re-run asserted to fail.
runPattern() {
	set -- $1
	[ "$#" -gt 0 ] || return 0
	printf '^(%s)$\n' "$(echo "$*" | tr ' ' '|')"
}

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
if [ -z "$(echo "$GAP_TRIPWIRE" | tr -d '[:space:]')" ]; then
	echo "known gaps: none — every measured Linux/libkrun gap has been closed"
fi
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
