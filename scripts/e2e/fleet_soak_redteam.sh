#!/usr/bin/env bash
# Red-team the fleet soak (MGIT-113): prove each invariant FAILS against a
# deliberately broken build.
#
# WHY THIS EXISTS. An unverified gate is a decoration. This repository has
# shipped two gates that were not actually running and one test that passed
# vacuously, and sandbox_registry_durability.sh earned its credibility by being
# run against a neutered watchdog and observed to go RED. A soak is worse than
# most gates in this respect: it asserts about the absence of bad states, and
# nearly every one of its assertions would also hold over a fleet that never
# started. So each invariant is checked against a build with exactly one defect
# injected, and the run is only credible if the soak fails AND names that
# invariant.
#
# HOW IT WORKS. For each case: copy the tree, apply one surgical patch, build
# mgit + mgit-sandboxd from the patched copy (signing the daemon on macOS,
# because an unsigned daemon cannot create a VM at all and would hand back a
# vacuous SKIP), run the soak against those binaries, and require that it exits
# non-zero with the expected invariant named in its output. A patched build that
# PASSES is reported as loudly as one that fails to build: it means the
# invariant does not actually depend on the code it claims to be about.
#
# This is not itself a gate — it takes a per-case build plus a full soak, so it
# runs when the soak's assertions change, and its results are recorded in
# docs/E2E-MATRIX.md. Nothing here modifies the working tree: every patch is
# applied to a throwaway copy under a temp directory.
#
# Usage: fleet_soak_redteam.sh [case...]
#   With no arguments every case runs. Case names are listed by --list.
#   Environment: the same guest-image variables the soak itself takes
#   (MGIT_GUEST_BASE / MGIT_GUEST_BIN_DIR / MGIT_GUEST_IMAGE / kernel+rootfs).
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/../.." && pwd -P)"

# Each case is: name | invariant it must break | the string the soak must print.
# The patch itself is a shell function named patch_<name>, applied inside the
# copied tree. Keeping the expected string here means a case cannot silently
# start passing for the wrong reason: it must break the RIGHT assertion.
CASES="reaping honesty capacity isolation trail ceiling"

case_invariant() {
	case "$1" in
	reaping) echo "INVARIANT I1 (REAPING) BROKE" ;;
	honesty) echo "INVARIANT I2 (HONESTY) BROKE" ;;
	capacity) echo "INVARIANT I3 (CAPACITY) BROKE" ;;
	isolation) echo "INVARIANT I4 (ISOLATION) BROKE" ;;
	trail) echo "INVARIANT I5 (TRAIL) BROKE" ;;
	ceiling) echo "I6" ;;
	esac
}

case_description() {
	case "$1" in
	reaping) echo "neuter the parent-death lifeline so a SIGKILLed daemon orphans its microVMs (the MGIT-103 defect)" ;;
	honesty) echo "make rehydration ADOPT every recorded sandbox without verifying it, so a dead VM is reported running (the MGIT-102 defect)" ;;
	capacity) echo "leak the admission reservation on release, so drained capacity is never returned (the MGIT-98 release path)" ;;
	isolation) echo "derive the per-sandbox state dir from a constant, so every sandbox shares one directory" ;;
	trail) echo "record every audit event twice, so a sandbox's history goes on after it ended" ;;
	ceiling) echo "disable the aggregate ceiling entirely, so no launch is ever refused" ;;
	esac
}

# --- the patches ------------------------------------------------------------
# Each removes ONE decision in the copied tree by injecting an early return at
# the top of the function that makes it. They are deliberately crude: the point
# is to delete the property under test, not to write plausible code.
#
# Every patch is anchored on the function's EXACT signature rather than a loose
# pattern. If a signature changes, the patch fails to apply and the harness
# reports it as a red-team FAILURE -- which is the desired behaviour, because a
# patch that silently stopped applying would quietly turn every case green.

# inject_after <file> <exact-signature-line> <code> inserts code immediately
# after the given line, failing if that line is not present exactly once.
inject_after() {
	local file="$1" sig="$2" code="$3" n
	n="$(grep -cF -- "$sig" "$file" || true)"
	[ "$n" = "1" ] || {
		echo "    anchor not found exactly once (${n:-0}x): $sig" >&2
		return 1
	}
	awk -v sig="$sig" -v code="$code" '
		{ print }
		index($0, sig) { print code }
	' "$file" >"$file.rt" && mv "$file.rt" "$file"
	grep -qF 'REDTEAM' "$file"
}

patch_reaping() {
	# The child watches the lifeline pipe it inherited and exits when the parent
	# goes. Returning before the watch is installed leaves the VM parentless and
	# running when its daemon is SIGKILLed -- exactly MGIT-103.
	inject_after internal/sandboxd/backend/libkrun/lifeline.go \
		'func installParentLifeline(lookup func(string) string, stderr io.Writer) {' \
		'	return // REDTEAM: lifeline disabled'
}

patch_honesty() {
	# verifyLocked asks the backend whether a recorded sandbox still exists.
	# Forcing ok=true adopts every row unverified, so a VM that died with its
	# daemon is re-reported as running -- present-and-dead, the MGIT-102 defect.
	inject_after internal/service/sandbox_rehydrate.go \
		'func (s *SandboxService) verifyLocked(ctx context.Context, row model.SandboxRegistration) (state string, booted, ok bool) {' \
		'	return row.Info.State, true, true // REDTEAM: verification disabled'
}

patch_capacity() {
	# unreserve returns the in-flight reservation to the pool. Making it a no-op
	# leaks it on every launch, so accounted capacity only ever grows and a
	# drained fleet cannot be refilled -- the MGIT-98 release path.
	inject_after internal/sandboxd/ceiling.go \
		'func (c *CeilingManager) unreserve(requestMB int) {' \
		'	return // REDTEAM: release path leaks'
}

patch_isolation() {
	# Destroying a sandbox stops leaving with its state directory, so every
	# create/destroy cycle strands one -- its overlay, its copy of the worktree
	# and its per-VM sockets -- under the daemon's work dir.
	#
	# NOTE ON WHY IT IS THIS DEFECT AND NOT A COLLIDING DIRECTORY. Making
	# SandboxStateDir return one shared path would be the more obvious injection,
	# but it cannot reach the I4 assertion: createOverlay opens overlay.img with
	# O_CREATE|O_EXCL, so the second sandbox fails to LAUNCH and the soak reports
	# a provisioning failure instead. That is the defence working, and it is
	# worth recording that a true state-dir collision is caught before it can
	# become a shared directory. The half of I4 that is independently violable
	# is the leak, so that is what this injects.
	sed -i.rt 's|if err := os.RemoveAll(sb.dir); err != nil {|if err := error(nil); err != nil { // REDTEAM: state dir leaked on destroy|' \
		internal/sandboxd/backend/microvm/manager.go
	rm -f internal/sandboxd/backend/microvm/manager.go.rt
	grep -q 'REDTEAM: state dir leaked on destroy' internal/sandboxd/backend/microvm/manager.go
}

patch_trail() {
	# Every audit event is written TWICE, so a life that ended goes on emitting:
	# `... killed killed`. That is the shape the ticket calls an unreadable
	# trail -- events recorded more than once, or interleaved, until a
	# per-sandbox history cannot be reconstructed from the table.
	#
	# NOTE ON WHY IT IS THIS DEFECT. The obvious injection is to DROP the
	# terminal event when a lost sandbox is discarded (the audit hole MGIT-102
	# closed). That was tried, and the soak does catch it -- but as INVARIANT I2,
	# which asserts the same "absent implies a terminal event" property one phase
	# earlier and therefore reaches it first. Catching it is the right outcome;
	# attributing it to I5 would not be. So I5 is proven against a violation only
	# it can see: ordering within a single sandbox's history.
	# One line: awk -v cannot carry a newline. The context value breaks the
	# recursion so each event is written exactly twice, not endlessly.
	inject_after internal/store/index/sandbox_events.go \
		'func (s *Store) AppendSandboxEvent(ctx context.Context, ev *model.SandboxEvent) error {' \
		'	if v, _ := ctx.Value(struct{ rt int }{}).(bool); !v { _ = s.AppendSandboxEvent(context.WithValue(ctx, struct{ rt int }{}, true), ev) } // REDTEAM: every event recorded twice'
}

patch_ceiling() {
	# admit is the refusal decision; always admitting removes the ceiling, so no
	# launch is ever refused however large the fleet grows.
	inject_after internal/sandboxd/ceiling.go \
		'func (c *CeilingManager) admit(count, usedMB, requestMB int) error {' \
		'	return nil // REDTEAM: ceiling disabled'
}

if [ "${1:-}" = "--list" ]; then
	for c in $CASES; do printf '  %-10s %s\n' "$c" "$(case_description "$c")"; done
	exit 0
fi

[ $# -gt 0 ] && CASES="$*"

# Sanity: the soak must be RUNNABLE here, or every case would "fail" by SKIP and
# prove nothing. Run it once against a clean build first.
echo "=== baseline: the soak must PASS against an unpatched build ==="
base="$(mktemp -d)"
trap 'rm -rf "$base"' EXIT
export PKG_CONFIG_PATH="${PKG_CONFIG_PATH:-}:$(brew --prefix libkrun 2>/dev/null)/lib/pkgconfig"

build_into() { # build_into <srcdir> <bindir>
	local src="$1" bin="$2"
	mkdir -p "$bin"
	(cd "$src" && CGO_ENABLED=0 go build -trimpath -o "$bin/mgit" ./cmd/mgit/) || return 1
	(cd "$src" && go build -trimpath -o "$bin/mgit-sandboxd" ./cmd/mgit-sandboxd/) || return 1
	# An UNSIGNED daemon cannot create a VM on macOS, so an unsigned red-team
	# build would fail for the wrong reason and look like a proven invariant.
	if [ "$(uname -s)" = "Darwin" ]; then
		codesign --force --sign - --entitlements "$src/build/darwin/vz.entitlements" \
			"$bin/mgit-sandboxd" >/dev/null 2>&1 || return 1
	fi
}

build_into "$repo" "$base/bin" || {
	echo "REDTEAM: baseline build failed"
	exit 1
}
if out="$(bash "$here/sandbox_fleet_soak.sh" "$base/bin" 2>&1)"; then
	case "$out" in
	*"SANDBOX FLEET SOAK: SKIP"*)
		echo "REDTEAM: the soak SKIPped on this host — no case can prove anything."
		printf '%s\n' "$out" | tail -3
		exit 1
		;;
	esac
	echo "  ok: baseline soak passes"
else
	echo "REDTEAM: the baseline soak FAILS unpatched — fix that before red-teaming."
	printf '%s\n' "$out" | tail -20
	exit 1
fi

# --- the cases --------------------------------------------------------------
failed=""
for c in $CASES; do
	want="$(case_invariant "$c")"
	echo
	echo "=== case '$c': $(case_description "$c") ==="
	echo "    must break: $want"
	src="$base/src-$c"
	# Copy the tree WITHOUT the mgit/git metadata: the patch must never be able
	# to touch the real repository.
	mkdir -p "$src"
	tar -cf - -C "$repo" --exclude .git --exclude .mgit --exclude .mtix . | tar -xf - -C "$src"
	(cd "$src" && "patch_$c") || {
		echo "  REDTEAM FAIL: patch '$c' did not apply — the code it targets moved"
		failed="$failed $c(patch)"
		continue
	}
	if ! build_into "$src" "$base/bin-$c" >/dev/null 2>&1; then
		echo "  REDTEAM FAIL: patched build for '$c' did not compile"
		failed="$failed $c(build)"
		continue
	fi
	if out="$(bash "$here/sandbox_fleet_soak.sh" "$base/bin-$c" 2>&1)"; then
		echo "  REDTEAM FAIL: the soak PASSED against a build with '$c' broken."
		echo "  That invariant is not actually asserted by anything."
		printf '%s\n' "$out" | tail -5
		failed="$failed $c(passed)"
		continue
	fi
	case "$out" in
	*"$want"*)
		echo "  ok: soak went RED naming '$want'"
		printf '%s\n' "$out" | grep -F "$want" | head -2 | sed 's/^/    /'
		;;
	*)
		echo "  REDTEAM FAIL: the soak failed, but NOT on '$want' — it may be"
		echo "  failing for an unrelated reason, which proves nothing about the invariant."
		printf '%s\n' "$out" | tail -6 | sed 's/^/    /'
		failed="$failed $c(wrong-invariant)"
		;;
	esac
done

echo
if [ -n "$failed" ]; then
	echo "FLEET SOAK REDTEAM: FAIL —$failed"
	exit 1
fi
echo "FLEET SOAK REDTEAM: PASS — every invariant went red against a build with that defect injected"
