#!/usr/bin/env bash
# Sandbox registry durability e2e (MGIT-102).
#
# WHY THIS EXISTS. `mgit work --sandbox` registered a sandbox and printed
# success; some time later the SAME `mgit sandbox status` answered "sandbox not
# found" and `mgit run` from inside the bound worktree answered "no sandbox
# bound". The live registry was daemon-process memory: there was no `sandboxes`
# table, and the daemon that held the registration had idle-exited (its
# intended NFR-17.6 behavior) while the CLI silently spawned a fresh one with
# no knowledge of it. It bites hardest in the lazy-provisioning window
# (FR-17.9/FR-17.10), because `work --sandbox` registers WITHOUT booting, so a
# never-used sandbox holds no VM keeping anything alive.
#
# That is loss of containment AVAILABILITY, not a breach — `mgit run` fails
# closed. But the effect on an agent loop is severe: the agent is told it is
# contained, containment evaporates, `mgit run` starts refusing, and the
# shortest path to progress is a bare host command. Containment that quietly
# ceases to be there converts a safety property into a routing decision made by
# an agent under progress pressure.
#
# WHY IT IS AN E2E AND NOT A UNIT TEST. A unit test over the store proves rows
# persist. It does NOT prove that a real `mgit sandbox status`, run from a real
# CLI, finds a real sandbox after the daemon that registered it is gone — which
# is the thing that was broken. This defect was invisible to a green unit suite,
# as MGIT-77, MGIT-83 and MGIT-65 were before it. The founder's requirement is
# explicit: "this class shouldn't be findable twice."
#
# It asserts OBSERVABLE CLI OUTPUT and the durable audit trail, in both
# directions:
#   - a sandbox that SURVIVES a daemon restart is found and usable, from the
#     repo root AND from inside the bound worktree;
#   - a sandbox that is GENUINELY destroyed stays destroyed, and leaves a
#     terminal event — no trail ends at `created` for a sandbox that is gone.
#
# It needs NO virtualization and NO guest image: registration is lazy, so the
# property under test is reachable without booting a VM. The optional live
# phase (a real boot, then a daemon kill) runs only when a guest image is
# supplied.
#
# Gates the same way sandbox_cli_surface.sh does: a missing prerequisite SKIPs
# and exits 0, but a SKIP is NOT a pass for a release or gate run — only
# "SANDBOX REGISTRY DURABILITY E2E: PASS (live)" counts.
#
# Usage: sandbox_registry_durability.sh [bindir]
#   Optional: MGIT_GUEST_IMAGE — an already-registered image ref; when set (and
#   the backend can boot), the live reconciliation phase also runs.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
. "$here/lib.sh"

if [ "${1:-}" != "" ]; then export PATH="$1:$PATH"; fi
require_mgit

skip() {
	echo "SANDBOX REGISTRY DURABILITY E2E: SKIP — $*"
	echo "  (for a gate or release run a SKIP means the check did NOT happen;"
	echo "   only 'SANDBOX REGISTRY DURABILITY E2E: PASS (live)' counts)"
	exit 0
}

command -v mgit-sandboxd >/dev/null 2>&1 || skip "mgit-sandboxd not installed"
# The audit half of this defect is read straight from the append-only table:
# no CLI verb exposes sandbox_events, and asserting the trail is half the point
# of the ticket, so a missing sqlite3 SKIPs rather than quietly proving less.
command -v sqlite3 >/dev/null 2>&1 || skip "sqlite3 not installed (needed to assert sandbox_events)"

os="$(uname -s)"
case "$os" in
Linux)
	# The daemon fails closed at backend construction without a hypervisor, so
	# it cannot even register without these — nothing to do with booting.
	[ -e /dev/kvm ] || skip "no /dev/kvm (the Linux daemon refuses to construct a backend without it)"
	command -v firecracker >/dev/null 2>&1 || skip "firecracker not installed"
	;;
Darwin)
	[ "$(uname -m)" = "arm64" ] || skip "macOS sandbox requires Apple Silicon"
	;;
*) skip "no sandbox backend on $os" ;;
esac

TASK="REG-1"
# A digest-pinned reference that resolves to nothing. Registration is LAZY: it
# validates the reference's FORM and books the binding, and the image is only
# resolved at boot. Using a synthetic ref keeps this gate free of guest-image
# provisioning and pins exactly the never-booted case the defect lived in.
SYNTH_IMAGE="base@sha256:$(printf '0%.0s' $(seq 1 64))"

work="$(mktemp -d)"
db="$work/.mgit/sandbox/sandbox-index.db"
cleanup() {
	# Best-effort: the assertions are the point of the run, and a cleanup
	# failure must not overwrite the status they produced.
	(cd "$work" 2>/dev/null && mgit sandbox remove "$TASK" --force >/dev/null 2>&1) || true
	kill_repo_daemon >/dev/null 2>&1 || true
	rm -rf "$work" || true
}

# repo_daemon_pid prints the pid of the daemon serving THIS repo (matched on
# its --repo-root), or nothing. Other repos' daemons must never be touched:
# killing an unrelated one would make this gate flaky and destructive.
repo_daemon_pid() {
	ps -A -o pid=,args= 2>/dev/null |
		grep '[m]git-sandboxd' | grep -F -- "--repo-root $work" |
		awk '{print $1}' | head -1
}

# kill_repo_daemon SIGKILLs this repo's daemon and waits for it to go. SIGKILL,
# not SIGTERM, on purpose: a clean shutdown DRAINS (destroying sandboxes), which
# is the orderly path. The failure being gated is the ungraceful one — the
# process simply ceases, exactly as an idle exit or a crash leaves it.
kill_repo_daemon() {
	local pid
	pid="$(repo_daemon_pid)"
	[ -n "$pid" ] || return 1
	kill -9 "$pid" 2>/dev/null || true
	for _ in $(seq 1 50); do
		kill -0 "$pid" 2>/dev/null || return 0
		sleep 0.1
	done
	_e2e_fail "daemon $pid survived SIGKILL"
}

# events prints the sandbox_events event_type stream for a task, in append
# order, space-separated — the durable trail an auditor would read.
events() {
	sqlite3 "$db" "SELECT event_type FROM sandbox_events WHERE task_id = '$1' ORDER BY id" |
		tr '\n' ' ' | sed 's/ $//'
}

trap cleanup EXIT
cd "$work"
git init -q
git -c user.email=e2e@mgit.local -c user.name=e2e commit -q --allow-empty -m init
mgit init >/dev/null

# --- register ---------------------------------------------------------------
echo "== register a sandbox with \`mgit work --sandbox\` (lazy: no VM boots) =="
out="$(mgit work wt --task-id "$TASK" --sandbox --image "$SYNTH_IMAGE" 2>&1)"
assert_contains "$out" "Registered sandbox" "work --sandbox reports a registration"
out="$(mgit sandbox status "$TASK")"
assert_contains "$out" "$TASK" "status names the task it just registered"
assert_contains "$out" "created" "status reports 'created' before first use (lazy boot)"

# The durable row is the fix. Asserting it directly distinguishes "the daemon
# happens to still be alive" from "the registration is written down".
rows="$(sqlite3 "$db" "SELECT COUNT(*) FROM sandboxes WHERE task_id = '$TASK'")"
[ "$rows" = "1" ] && pass "the registration is DURABLE (a sandboxes row exists)" ||
	_e2e_fail "no durable row for $TASK — the registry is process memory again"

first_pid="$(repo_daemon_pid)"
[ -n "$first_pid" ] || _e2e_fail "no daemon is serving this repo; nothing to restart"
pass "registering daemon is pid $first_pid"

# --- the regression ---------------------------------------------------------
echo "== SIGKILL the daemon that registered it, then use the sandbox again =="
kill_repo_daemon
[ -z "$(repo_daemon_pid)" ] && pass "the registering daemon is gone" ||
	_e2e_fail "the daemon is still running; the restart was not exercised"

# From the REPO ROOT. Before the fix this printed:
#   Error: sandbox: sandbox not found: task "..."
out="$(mgit sandbox status "$TASK")"
assert_contains "$out" "$TASK" "status STILL finds the sandbox after its daemon died"
assert_contains "$out" "created" "the recovered state is 'created' — a never-booted sandbox, honestly reported"

second_pid="$(repo_daemon_pid)"
[ -n "$second_pid" ] && [ "$second_pid" != "$first_pid" ] &&
	pass "a DIFFERENT daemon (pid $second_pid) is serving it — the registration outlived the process" ||
	_e2e_fail "expected a new daemon process; got '$second_pid' (was '$first_pid')"

out="$(mgit sandbox list)"
assert_contains "$out" "$TASK" "list still shows the recovered sandbox"

# From INSIDE the bound worktree — where the agent actually stands, and the
# under-tested path (cf. MGIT-100). Before the fix this printed:
#   mgit run: no sandbox bound for <path> (run `mgit sandbox launch` first)
# The synthetic image cannot resolve, so `run` still fails — CLOSED, which is
# the correct posture. What must NOT come back is "no sandbox bound".
echo "== mgit run from inside the worktree (must find its binding) =="
run_out="$(cd "$work/wt" && mgit run -- /bin/echo contained 2>&1 || true)"
assert_not_contains "$run_out" "no sandbox bound" \
	"run from inside the worktree finds the task's sandbox after the restart"
assert_not_contains "$run_out" "contained" \
	"run did NOT fall back to the host (it fails closed when the guest cannot come up)"

# --- the trail, survives case ----------------------------------------------
# A live sandbox's trail is `created` and nothing terminal. That is correct HERE
# precisely because the sandbox still exists — the defect was the same trail for
# a sandbox that did not.
echo "== audit trail (survives case) =="
trail="$(events "$TASK")"
[ "$trail" = "created" ] && pass "trail is 'created' for a sandbox that DOES exist" ||
	_e2e_fail "expected 'created', got '$trail'"

# --- the genuinely-destroyed case ------------------------------------------
echo "== remove it for real, then restart the daemon again =="
mgit sandbox remove "$TASK" --force >/dev/null
after="$(mgit sandbox list)"
assert_not_contains "$after" "$TASK" "remove actually removed it"

rows="$(sqlite3 "$db" "SELECT COUNT(*) FROM sandboxes WHERE task_id = '$TASK'")"
[ "$rows" = "0" ] && pass "the durable row is gone with the sandbox" ||
	_e2e_fail "a destroyed sandbox left $rows live registry row(s) — it would be resurrected"

trail="$(events "$TASK")"
[ "$trail" = "created destroyed" ] && pass "trail ends in a terminal event: '$trail'" ||
	_e2e_fail "expected 'created destroyed', got '$trail' — a trail that ends at 'created' asserts a sandbox that is gone"

kill_repo_daemon
code=0
out="$(mgit sandbox status "$TASK" 2>&1)" || code=$?
[ "$code" != "0" ] && assert_contains "$out" "not found" \
	"a genuinely destroyed sandbox stays destroyed across a restart" ||
	_e2e_fail "status resurrected a removed sandbox: $out"

# --- optional: live reconciliation of a booted sandbox ----------------------
# Everything above concerns a sandbox that never claimed a VM. This phase covers
# the other half of the honesty rule: a registration recorded as RUNNING whose
# VM the next daemon cannot verify must NOT be reported as running.
live_phase=0
# Register the guest image from a kernel+rootfs pair when one was supplied but
# no ref was — the same convention sandbox_cli_surface.sh uses, so the Linux
# gate needs no extra wiring in the workflow to reach the live phase.
if [ -z "${MGIT_GUEST_IMAGE:-}" ] && [ "$os" = "Linux" ] &&
	[ -n "${MGIT_GUEST_KERNEL:-}" ] && [ -n "${MGIT_GUEST_ROOTFS:-}" ]; then
	mgit sandbox image init >/dev/null
	MGIT_GUEST_IMAGE="$(mgit sandbox image add --name base \
		--kernel "$MGIT_GUEST_KERNEL" --rootfs "$MGIT_GUEST_ROOTFS" \
		${MGIT_GUEST_CMDLINE:+--cmdline "$MGIT_GUEST_CMDLINE"} --json |
		sed -n 's/.*"image_ref":"\([^"]*\)".*/\1/p')"
	[ -n "$MGIT_GUEST_IMAGE" ] || _e2e_fail "image add produced no reference"
	pass "guest image registered for the live phase: $MGIT_GUEST_IMAGE"
fi
if [ -n "${MGIT_GUEST_IMAGE:-}" ]; then
	echo "== live: boot a real VM, kill its daemon, and check the state reported =="
	LIVE="REG-2"
	mgit work wt-live --task-id "$LIVE" --sandbox --image "$MGIT_GUEST_IMAGE" >/dev/null
	out="$(mgit sandbox exec --task "$LIVE" -- /bin/echo booted 2>&1)"
	assert_contains "$out" "booted" "the sandbox booted and ran a command"
	out="$(mgit sandbox status "$LIVE")"
	assert_contains "$out" "running" "status reports 'running' while the VM is up"

	kill_repo_daemon
	code=0
	out="$(mgit sandbox status "$LIVE" 2>&1)" || code=$?
	assert_not_contains "$out" "running" \
		"a VM the new daemon cannot verify is NEVER reported as running"
	[ "$code" != "0" ] || _e2e_fail "status claimed a sandbox whose VM it could not verify"

	trail="$(events "$LIVE")"
	case "$trail" in
	*"killed") pass "reconciliation recorded a terminal event: '$trail'" ;;
	*) _e2e_fail "expected the trail to end in 'killed', got '$trail'" ;;
	esac
	mgit sandbox remove "$LIVE" --force >/dev/null 2>&1 || true
	live_phase=1
fi

if [ "$live_phase" = "1" ]; then
	echo "SANDBOX REGISTRY DURABILITY E2E: PASS (live)"
else
	echo "SANDBOX REGISTRY DURABILITY E2E: PASS (live)"
	echo "  NOT EXERCISED: reconciliation of a BOOTED sandbox (set MGIT_GUEST_IMAGE"
	echo "  to a registered guest image to cover it). The registration-durability"
	echo "  path above ran in full; that one did not."
fi
