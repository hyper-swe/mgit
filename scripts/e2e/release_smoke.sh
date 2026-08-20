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
	# -t 300 is the MGIT-143 rule on this step's history (worst success 62s,
	# n=135). The restore matters: a half-written archive left in $WORK would
	# be picked up by the `find` below and fail `tar` identically on every
	# attempt. Refs: MGIT-143 clause 3
	"$(dirname "$0")/../ci/guard-fetch.sh" -t 300 -l release-archive-download \
		-c "rm -f '$WORK'/mgit_*_darwin_arm64.tar.gz" -- \
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
# LIVENESS FIRST: what can run at all, unquarantined. Everything after this
# depends on knowing that, and the first version of this script did not — it
# asserted the daemon must run, then reported "the archive is broken" on a
# runner that simply had no libkrun.
#
# Only flags that have existed in every release are used. Binding liveness to a
# newer flag is what made a missing feature indistinguishable from a Gatekeeper
# kill (MGIT-83).
# ---------------------------------------------------------------------------
echo "== the shipped binaries run =="
if ! mgit_out="$("$ext/mgit" --version 2>&1)"; then
	_e2e_fail "mgit does not run: $mgit_out — the archive is broken"
fi
pass "mgit ran: $mgit_out"

# mgit-sandboxd LINKS libkrun by absolute path on macOS (verified with otool:
# /opt/homebrew/opt/libkrun/lib/libkrun.1.dylib). On a host without it, dyld
# aborts before main — "Abort trap: 6" with a Library-not-loaded message. That
# is MGIT-75's deliberate fail-closed design, not a broken archive: core mgit is
# CGO-free and links nothing. Distinguish the two by the loader's own words
# rather than by guessing from the exit status.
daemon_runs=0
if sbx_err="$("$ext/mgit-sandboxd" --help 2>&1 >/dev/null)"; then
	daemon_runs=1
	pass "mgit-sandboxd ran"
elif printf '%s' "$sbx_err" | grep -q 'Library not loaded'; then
	skip_note "mgit-sandboxd cannot load libkrun on this host, by design (MGIT-75) —"
	echo "        core mgit is unaffected. Install libkrun to cover the daemon here."
else
	_e2e_fail "mgit-sandboxd does not run, and not for a missing libkrun: $sbx_err"
fi

# ---------------------------------------------------------------------------
# BUILD AGREEMENT. A different question from liveness, and the only one allowed
# to depend on a feature — so the capability is probed, not assumed from the tag.
# ---------------------------------------------------------------------------
if [ "$daemon_runs" = "1" ] && sbx_out="$("$ext/mgit-sandboxd" --version 2>/dev/null)"; then
	mgit_build="${mgit_out#mgit version }"
	sbx_build="${sbx_out#mgit-sandboxd version }"
	if [ "$mgit_build" != "$sbx_build" ]; then
		_e2e_fail "the archive was assembled from two different builds:
      mgit:          $mgit_build
      mgit-sandboxd: $sbx_build"
	fi
	pass "both binaries report one build: $mgit_build"
elif [ "$daemon_runs" = "1" ]; then
	skip_note "this daemon predates --version (added in MGIT-83) — build agreement not checkable for $TAG"
else
	skip_note "build agreement needs a runnable daemon (see above)"
fi

# ---------------------------------------------------------------------------
# Gatekeeper quarantine, asserted in BOTH directions — but ONLY on a host that
# actually enforces it.
#
# THIS CANNOT BE VERIFIED IN CI, and pretending otherwise produced two false
# conclusions on this job's first run: a quarantined binary RAN (so the check
# announced that notarization had landed) and the daemon "did not run" (so the
# check credited Gatekeeper for what was really a missing dylib). A hosted
# runner has no logged-in user session and reports `spctl --status: assessments
# disabled`, so it cannot demonstrate the kill. Gate on the assessment state
# rather than on the environment being "CI", because that is the property that
# actually matters.
#
# What is true on an enforcing host, measured on the published v0.4.3 archive:
# a quarantined ad-hoc-signed binary is SIGKILLed, and the same binary runs once
# the attribute is removed. That is MGIT-64's documented state pending
# notarization — so this asserts reality, not a wish. A quarantined binary that
# RUNS is reported as news worth acting on.
# ---------------------------------------------------------------------------
echo "== Gatekeeper quarantine (MGIT-64) =="
if [ "$(uname -s)" != "Darwin" ]; then
	skip_note "not macOS — Gatekeeper quarantine is darwin-only"
elif [ "${MGIT_SMOKE_QUARANTINE:-0}" != "1" ]; then
	# Off by default on a human's machine: exercising this raises a real
	# "cannot verify ... free of malware" alert, and desensitising a developer
	# to malware alerts is a worse outcome than skipping one check on a laptop.
	skip_note "quarantine kill-path check is opt-in (set MGIT_SMOKE_QUARANTINE=1) and"
	echo "        real-Mac only; it fires genuine macOS malware alerts, so it is never"
	echo "        run unattended and never run in CI. This is checklist step 7."
elif [ -n "${CI:-}" ]; then
	# Belt and braces: the workflow does not opt in, and if some future job
	# does, this refuses anyway. `spctl --status` is NOT a sufficient gate —
	# measured: a hosted macOS runner reports "assessments enabled" and still
	# runs a quarantined ad-hoc-signed binary.
	skip_note "refusing to draw a Gatekeeper conclusion in CI — a hosted runner reports"
	echo "        'assessments enabled' and runs quarantined binaries anyway. Real-Mac only."
else
	echo "  note: this EXECUTES a quarantined binary; macOS will raise a malware alert."
	# Only binaries proven runnable above: a daemon that cannot load libkrun
	# would "fail" this for a reason that has nothing to do with Gatekeeper.
	qbins="mgit"
	[ "$daemon_runs" = "1" ] && qbins="mgit mgit-sandboxd"
	for b in $qbins; do
		if ! xattr -p com.apple.quarantine "$ext/$b" >/dev/null 2>&1; then
			xattr -w com.apple.quarantine "0081;00000000;release_smoke;" "$ext/$b"
			_quarantined="$_quarantined $ext/$b"
		fi
		if run_bounded 60 "$ext/$b" --help; then
			# Do NOT announce that notarization has landed. Measured: a GitHub
			# macOS runner reports "assessments enabled" and runs a quarantined
			# ad-hoc-signed binary anyway, so this outcome does not distinguish
			# "Gatekeeper stopped killing it" from "this host never would have".
			# Either way a human has to look, so it is counted as a skip rather
			# than passed over with a confident-sounding note.
			skip_note "quarantined $b RAN — INCONCLUSIVE. Either this host does not"
			echo "        enforce the kill (hosted runners do not, despite spctl saying"
			echo "        'enabled'), or notarization landed and MGIT-64 + INSTALL-SANDBOX.md"
			echo "        need updating. Re-run on a real, interactive Mac to tell which."
		else
			pass "quarantined $b did not run (killed or blocked), as MGIT-64 documents"
		fi
		xattr -d com.apple.quarantine "$ext/$b" 2>/dev/null || true
	done
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
	#
	# fetch-guard: this curl IS the assertion, not a supply fetch -- it asks
	# whether the tap is reachable WITHOUT credentials (MGIT-66). Retrying it
	# would blur the very signal it exists to read, and it already carries its
	# own bound via `|| echo 000` plus curl's default connect timeout.
	# Refs: MGIT-143
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
