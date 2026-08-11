#!/usr/bin/env bash
# Post-publish release smoke: the scriptable half of RELEASE-CHECKLIST steps
# 6-8, run against a PUBLISHED release rather than a build tree.
#
# WHY THIS IS A SCRIPT. Steps 6-8 were prose, and prose drifted from the
# binaries four times in one release cycle (MGIT-84): a flag the daemon did not
# have, then the same flag reinstated against a release that predates it, a
# missing MGIT_GUEST_CMDLINE that made a guest boot with no root= and no init=,
# and a "CI cannot run these" claim that had been false since hosted runners
# gained /dev/kvm. Nothing executed the prose, so nothing noticed. The parts of
# the release gate that ARE scripts have never drifted, because CI runs them.
#
# WHAT IT DELIBERATELY DOES NOT CLAIM. The true Gatekeeper reproduction needs a
# real download onto a Mac that did not build the release; `scp` and a build
# directory never set com.apple.quarantine, which is exactly how this shipped
# broken once (MGIT-64). This script makes the kill path DETERMINISTIC instead
# of hoping for it: if the extracted binaries are not quarantined it sets the
# attribute itself and says so. That is a stronger check than "we downloaded it
# and hoped", but it is not proof that a browser download quarantines the same
# way — the checklist keeps that as a human step and says why.
#
# CAPABILITY IS DERIVED FROM THE ARTIFACT, NEVER FROM A VERSION LIST. The
# daemon gained --version in MGIT-83; older releases exit 2 on it. This script
# asks the binary whether it supports the flag rather than consulting a table
# of versions, because a maintained list of "which release has what" is one
# more thing that drifts — the defect this whole ticket is about.
#
# Usage:
#   scripts/e2e/release_smoke.sh <tag> [workdir]
#   MGIT_SMOKE_ARCHIVE=/path/to/mgit_x_darwin_arm64.tar.gz scripts/e2e/release_smoke.sh <tag>
#
# Refs: MGIT-84, MGIT-83, MGIT-64, MGIT-65, MGIT-61.14
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
. "$here/lib.sh"

TAG="${1:-}"
[ -n "$TAG" ] || {
	echo "usage: $0 <tag> [workdir]   (e.g. $0 v0.4.3)" >&2
	exit 2
}
WORK="${2:-$(mktemp -d)}"
mkdir -p "$WORK"

# The synthetic quarantine attribute this script sets must be removed on EVERY
# exit path, not just the happy one. The first version cleared it inline after
# the check and then failed before reaching that line, leaving two quarantined
# binaries behind — and because macOS raises a user-visible "cannot verify this
# app is free of malware" alert when a quarantined binary is executed, the
# operator got a malware warning from a test they were not running. A cleanup
# that only happens on success is not cleanup.
_quarantined=""
cleanup_quarantine() {
	for _q in $_quarantined; do
		[ -e "$_q" ] && xattr -d com.apple.quarantine "$_q" 2>/dev/null || true
	done
}
trap cleanup_quarantine EXIT

# A skip is LOUD and non-zero-worthy in a release context: this script exists to
# be run deliberately, so "could not check" must never read like "checked".
skipped=0
skip_note() {
	echo "  SKIP: $*"
	skipped=$((skipped + 1))
}

# run_bounded <seconds> <cmd...> — run a command with a hard time bound.
#
# Executing a QUARANTINED binary is not a fast, predictable failure. Gatekeeper
# performs an assessment first, which can reach out to Apple, and for a binary
# it has never seen it can block for a long time or wait on a UI prompt that
# will never be answered on a runner. Measured: this hung past ten minutes
# against a freshly published v0.4.4 while v0.4.3 (already assessed on that
# host) was killed instantly. Unbounded, that turns a check into a stuck job.
#
# macOS ships no coreutils `timeout`, so this is the portable form.
run_bounded() {
	_secs="$1"
	shift
	"$@" >/dev/null 2>&1 &
	_pid=$!
	( sleep "$_secs"; kill -9 "$_pid" 2>/dev/null ) 2>/dev/null &
	_killer=$!
	if wait "$_pid" 2>/dev/null; then _rc=0; else _rc=$?; fi
	kill "$_killer" 2>/dev/null || true
	return "$_rc"
}

echo "== release smoke for $TAG =="

# ---------------------------------------------------------------------------
# Step 7 (a): obtain the darwin/arm64 archive — the platform where Gatekeeper,
# the entitlement signing and the libkrun backend all apply at once.
# ---------------------------------------------------------------------------
archive="${MGIT_SMOKE_ARCHIVE:-}"
if [ -z "$archive" ]; then
	command -v gh >/dev/null 2>&1 || _e2e_fail "gh is required to download the release (or set MGIT_SMOKE_ARCHIVE)"
	echo "== downloading $TAG darwin/arm64 archive =="
	gh release download "$TAG" -p 'mgit_*_darwin_arm64.tar.gz' -D "$WORK" >/dev/null
	archive="$(find "$WORK" -name 'mgit_*_darwin_arm64.tar.gz' -maxdepth 1 | head -1)"
fi
[ -n "$archive" ] && [ -f "$archive" ] || _e2e_fail "no darwin/arm64 archive for $TAG"
pass "archive: $(basename "$archive")"

ext="$WORK/extracted"
rm -rf "$ext"
mkdir -p "$ext"
tar -xzf "$archive" -C "$ext"
assert_file "$ext/mgit" "archive ships mgit"
assert_file "$ext/mgit-sandboxd" "archive ships mgit-sandboxd (the sandbox is unusable without it)"
# MGIT-65: without guest/, an installed mgit cannot compose a guest base at all.
assert_file "$ext/guest/mgit" "archive ships guest/mgit"
assert_file "$ext/guest/mgit-guest" "archive ships guest/mgit-guest"

# ---------------------------------------------------------------------------
# Step 7 (b+c): Gatekeeper quarantine, asserted in BOTH directions.
#
# What is true today, measured on the published v0.4.3 archive: a quarantined
# ad-hoc-signed binary is SIGKILLed (exit 137, "Killed: 9"), and the same
# binary runs normally once the attribute is removed. That is MGIT-64's
# documented state — the remedy is `xattr -d com.apple.quarantine`, and the
# real fix (Developer ID notarization) is an owner decision that has not been
# taken.
#
# So this check asserts REALITY, not a wish. The checklist used to demand that
# both binaries "run without the remedy", which is a property the project
# knowingly does not have — a gate that cannot pass teaches an operator to
# ignore it. Instead:
#
#   1. quarantined  -> expected to be killed. If it RUNS, that is good news,
#      not a failure: notarization or a policy change landed, and MGIT-64,
#      INSTALL-SANDBOX.md and this script all need updating. Reported loudly.
#   2. after remedy -> MUST run. This is the assertion that actually protects
#      users: it proves the archive shipped working binaries, so a real
#      failure (bad signature, wrong arch, truncated upload) is distinguished
#      from the quarantine everyone already knows about.
# ---------------------------------------------------------------------------
echo "== Gatekeeper quarantine, both directions (MGIT-64) =="
if [ "$(uname -s)" != "Darwin" ]; then
	skip_note "not macOS — Gatekeeper quarantine is darwin-only"
elif [ "${MGIT_SMOKE_QUARANTINE:-0}" != "1" ]; then
	# OFF BY DEFAULT ON A HUMAN'S MACHINE, deliberately. Executing a quarantined
	# binary makes macOS raise a "cannot verify ... free of malware" alert, and
	# there is no way to exercise the kill path without triggering it. Firing
	# real malware alerts at an operator during a routine release check teaches
	# them to dismiss malware alerts — a worse outcome than not running this one
	# check locally. CI sets MGIT_SMOKE_QUARANTINE=1, where the alert has no one
	# to desensitise. Refs: MGIT-84, MGIT-64
	skip_note "quarantine kill-path check is opt-in locally (set MGIT_SMOKE_QUARANTINE=1);"
	echo "        it fires real macOS malware alerts, so it runs unattended in CI instead"
else
	echo "  note: this check EXECUTES quarantined binaries, so macOS will raise a"
	echo "        \"cannot verify ... free of malware\" alert for each one. That alert"
	echo "        is this test working. The attribute is removed on every exit path."
	for b in mgit mgit-sandboxd; do
		if ! xattr -p com.apple.quarantine "$ext/$b" >/dev/null 2>&1; then
			xattr -w com.apple.quarantine "0081;00000000;release_smoke;" "$ext/$b"
			_quarantined="$_quarantined $ext/$b"
		fi
		if run_bounded 60 "$ext/$b" --help; then
			echo "  NOTE: quarantined $b RAN. The documented Gatekeeper kill no longer"
			echo "        reproduces — notarization or a policy change has landed."
			echo "        Update MGIT-64, docs/INSTALL-SANDBOX.md and this check."
		else
			# Killed outright, or blocked by an assessment that never resolved.
			# Both mean the same thing to a user: the binary did not run.
			pass "quarantined $b did not run (killed or blocked), as MGIT-64 documents"
		fi
		xattr -d com.apple.quarantine "$ext/$b" 2>/dev/null || true
	done
fi

# ---------------------------------------------------------------------------
# Step 7 (c): the binaries themselves work. Every probe here must run on every
# release ever shipped, so it uses only flags that have always existed —
# binding liveness to a newer flag is what made a missing feature
# indistinguishable from a Gatekeeper kill (MGIT-83).
# ---------------------------------------------------------------------------
echo "== the shipped binaries run (quarantine removed) =="
if ! mgit_out="$("$ext/mgit" --version 2>&1)"; then
	_e2e_fail "mgit does not run even unquarantined: $mgit_out — the archive is broken"
fi
pass "mgit ran: $mgit_out"
if ! "$ext/mgit-sandboxd" --help >/dev/null 2>&1; then
	_e2e_fail "mgit-sandboxd does not run even unquarantined — the archive is broken"
fi
pass "mgit-sandboxd ran"

# ---------------------------------------------------------------------------
# Step 7 (d): BUILD AGREEMENT. A different question from liveness, and the only
# one allowed to depend on a feature — so the capability is probed, not assumed
# from the tag.
# ---------------------------------------------------------------------------
if sbx_out="$("$ext/mgit-sandboxd" --version 2>/dev/null)"; then
	# Compare the build metadata, ignoring each binary's own name prefix.
	mgit_build="${mgit_out#mgit version }"
	sbx_build="${sbx_out#mgit-sandboxd version }"
	if [ "$mgit_build" != "$sbx_build" ]; then
		_e2e_fail "the archive was assembled from two different builds:
      mgit:          $mgit_build
      mgit-sandboxd: $sbx_build"
	fi
	pass "both binaries report one build: $mgit_build"
else
	skip_note "this daemon predates --version (added in MGIT-83) — build agreement not checkable for $TAG"
fi

# ---------------------------------------------------------------------------
# Step 8: libkrun networking capability. libkrun gates its net API behind an
# opt-in build flag; without it the VMM falls back to TSI and a guest gets full
# host egress, so mgit-sandboxd refuses to start. The libkrun/krun tap passes
# NET=1 today, but that is a THIRD-PARTY build flag and this is the check that
# notices if it ever stops.
# ---------------------------------------------------------------------------
echo "== libkrun networking capability (SEC-04) =="
if [ "$(uname -s)" != "Darwin" ]; then
	skip_note "not macOS — libkrun is the macOS backend"
elif ! command -v brew >/dev/null 2>&1 || ! brew --prefix libkrun >/dev/null 2>&1; then
	skip_note "libkrun not installed — cannot verify NET=1 (install it to check the macOS sandbox path)"
elif nm -gU "$(brew --prefix libkrun)/lib/libkrun.dylib" 2>/dev/null | grep -q krun_add_net_unixgram; then
	pass "libkrun exports krun_add_net_unixgram (built with NET=1)"
else
	_e2e_fail "libkrun does NOT export krun_add_net_unixgram — built without NET=1; every macOS sandbox would fail closed"
fi

# ---------------------------------------------------------------------------
# Step 6: the Homebrew channel. Verified against whatever is installed, because
# the install itself mutates the operator's machine and is the human's call.
# ---------------------------------------------------------------------------
# ---------------------------------------------------------------------------
# The tap must be reachable WITHOUT credentials. It was private once, and every
# person who tested had credentials, so `brew tap` worked for them and failed
# for every external user (MGIT-66). An authenticated check cannot see this;
# only an anonymous fetch can.
# ---------------------------------------------------------------------------
echo "== homebrew tap is reachable unauthenticated (MGIT-66) =="
if ! command -v curl >/dev/null 2>&1; then
	skip_note "curl not present — cannot make an anonymous request"
else
	# -H bypasses any ambient credential helper; a 404 here means private.
	code="$(curl -s -o /dev/null -w '%{http_code}' -H 'Authorization:' \
		https://raw.githubusercontent.com/hyper-swe/homebrew-tap/main/Formula/mgit.rb || echo 000)"
	if [ "$code" = "200" ]; then
		pass "the tap formula is publicly fetchable (HTTP $code)"
	else
		_e2e_fail "the Homebrew tap is NOT publicly reachable (HTTP $code).
      Every external user's \`brew install hyper-swe/tap/mgit\` fails, while it
      keeps working for anyone with credentials — which is how this shipped
      broken once. Make hyper-swe/homebrew-tap public."
	fi
fi

echo "== homebrew channel =="
if ! command -v brew >/dev/null 2>&1; then
	skip_note "brew not present"
elif ! brew list --versions mgit >/dev/null 2>&1; then
	skip_note "mgit is not brew-installed — run 'brew install hyper-swe/tap/mgit' to cover step 6"
else
	brew_ver="$(brew list --versions mgit | awk '{print $2}')"
	assert_ok "brew-installed mgit runs" -- "$(brew --prefix)/bin/mgit" --version
	assert_ok "brew-installed mgit-sandboxd runs" -- "$(brew --prefix)/bin/mgit-sandboxd" --help
	assert_file "$(brew --prefix mgit)/libexec/guest/mgit-guest" \
		"brew install ships guest/ (MGIT-65: without it 'sandbox base from' cannot compose)"
	case "$TAG" in
	*"$brew_ver"*) pass "brew has $brew_ver, matching $TAG" ;;
	*) skip_note "brew has $brew_ver, not $TAG — the tap may not have updated yet" ;;
	esac
fi

echo
if [ "$skipped" -gt 0 ]; then
	echo "RELEASE SMOKE: PASS with $skipped SKIPPED check(s) — a skip is NOT a pass;"
	echo "  each one above says what could not be verified and on what host it would be."
else
	echo "RELEASE SMOKE: PASS (all checks ran)"
fi
