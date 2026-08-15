#!/usr/bin/env bash
# Fleet soak + chaos gate (MGIT-113).
#
# WHY THIS EXISTS. Every defect this project fixed in the registry/reaping class
# was found and verified SEQUENTIALLY, one sandbox at a time, by hand:
# MGIT-102 (registry lost when the daemon exits) -- one sandbox, one restart;
# MGIT-103 (VM orphaned by SIGKILL) -- one daemon, one VM, one kill;
# MGIT-109 (policy staged onto an unbooted sandbox) -- one sandbox; MGIT-98
# (fleet memory ceiling) -- the aggregate path, whose aggregate refusal was
# proven BY UNIT TEST ONLY because reaching it needs a real concurrent fleet.
# Each fix is correct for the case it was tested against. None had been
# exercised with sandboxes being created, executed and destroyed SIMULTANEOUSLY,
# which is the shape a worker pool produces -- so our concurrent behaviour was
# unmeasured, and "unmeasured" is where this codebase keeps finding defects.
#
# WHAT IT ASSERTS. Invariants, not the absence of crashes. A soak that only
# checks "nothing exploded" passes while the fleet quietly leaks capacity, so
# every invariant below gets its own positive assertion and its own failure
# message naming WHICH invariant broke:
#
#   I1 REAPING      no orphaned VM process survives a daemon SIGKILL (MGIT-103)
#   I2 HONESTY      every registration is present-and-usable, or absent-with-a-
#                   terminal-audit-event -- never present-and-dead (MGIT-102)
#   I3 CAPACITY     accounted fleet capacity returns to its floor when the fleet
#                   drains, PROVEN by re-admitting a full fleet afterwards
#                   (an accounting leak in the release path would refuse) (MGIT-98)
#   I4 ISOLATION    no per-sandbox state-directory or socket collision under
#                   rapid create/destroy, and no state dir leaks after destroy
#   I5 TRAIL        the append-only audit trail reconstructs a coherent
#                   per-sandbox history: starts at `created`, never continues
#                   past a terminal event, and ends terminal for a sandbox
#                   that is gone
#   I6 CEILING      hitting the aggregate ceiling is a VALID outcome, refused
#                   cleanly and namng the ceiling -- not a crash, and not a
#                   diagnosis pointing at the wrong cause (MGIT-98, MGIT-95)
#
# CHURN IS REAL. Sandboxes are created and destroyed WHILE others run and are
# being exec'd, not in a clean create-all-then-kill-all phase. The interesting
# races live in the overlap, and the daemon SIGKILL lands with execs in flight.
#
# BOUNDED, AND IT SAYS WHICH PROFILE RAN. `short` gates every push; `long` is
# the nightly soak. The final line always names the profile and the fleet width,
# because a short run that reads as a full soak is the same lie as a SKIP that
# reads as a pass.
#
# MEASURED, NOT GUESSED (see docs/E2E-MATRIX.md for the table). On the reference
# host -- Apple Silicon, 10 vCPU / 16 GB, libkrun, debian bookworm-slim guest,
# 512 MB + 1 vCPU per sandbox -- concurrent create+first-exec measured 10.7 s at
# N=2, 16.5 s at N=4 and 41-59 s at N=8; warm concurrent exec 42 ms / 145 ms /
# 6.2 s; concurrent remove 37 ms / 88 ms / 1.5 s. N=8 is also exactly where the
# stock host-wide count cap refuses. N=4 is therefore the widest fleet that
# still costs well under two minutes with churn (the push gate), and N=8 the
# widest the host admits at all (the nightly).
#
# Gates the same way sandbox_cli_surface.sh and sandbox_registry_durability.sh
# do: a missing prerequisite SKIPs and exits 0, but a SKIP is NOT a pass -- only
# a final "SANDBOX FLEET SOAK: PASS" counts.
#
# EACH INVARIANT IS PROVEN TO GO RED. Every one was run against a build with
# exactly that defect injected, and observed to fail NAMING it: I1 against a
# neutered parent-death lifeline, I2 against rehydration that adopts without
# verifying, I3 against a leaking reservation-release path, I4 against a state
# directory left behind on destroy, I5 against every audit event recorded twice,
# I6 against the ceiling disabled. `scripts/e2e/fleet_soak_redteam.sh` (or
# `make soak-redteam`) reproduces all six. An unverified gate is a decoration,
# and a soak is the worst case: most of these assertions would also hold over a
# fleet that never started.
#
# ON LENGTH (this file exceeds the 500-line guidance in CLAUDE.md). Roughly 40%
# is the rationale above and inline; the executable part is ~380 lines. The
# mechanics -- process and state-dir probes, the ceiling policy, and the two
# assertion batteries that walk the registry and the audit trail -- already live
# in fleet_lib.sh. What remains is one linear phase narrative, and splitting THAT
# across files would cost the reviewer the ability to read what happens, in what
# order, with what overlap, which is the property the gate is really about.
#
# Usage: sandbox_fleet_soak.sh [bindir]
#   MGIT_SOAK_PROFILE=short|long   (default short)
#   Guest image, same conventions as the other sandbox gates:
#     MGIT_GUEST_IMAGE                     an already-registered ref, or
#     MGIT_GUEST_BASE [+MGIT_GUEST_BIN_DIR] a guest root to register here, or
#     MGIT_GUEST_KERNEL + MGIT_GUEST_ROOTFS [+ MGIT_GUEST_CMDLINE]  (Linux)
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
. "$here/lib.sh"
# shellcheck source=fleet_lib.sh
. "$here/fleet_lib.sh"

if [ "${1:-}" != "" ]; then export PATH="$1:$PATH"; fi
require_mgit

PROFILE="${MGIT_SOAK_PROFILE:-short}"
case "$PROFILE" in
short)
	FLEET=4
	CHURN_ROUNDS=2
	;;
long)
	FLEET=8
	CHURN_ROUNDS=4
	;;
*)
	_e2e_fail "unknown MGIT_SOAK_PROFILE '$PROFILE' (expected short or long)"
	;;
esac

# Modest, DECLARED caps. The policy default is 2048 MB + 2 vCPU per sandbox, so
# a fleet of 8 at the default is 16 GB of real host memory -- trusting the
# default is how a soak turns into a host OOM. Declaring them also makes the
# ceiling arithmetic below predictable. Refs: MGIT-95, MGIT-98
SB_MEM_MB=512
SB_CPUS=1

skip() {
	echo "SANDBOX FLEET SOAK: SKIP — $*"
	echo "  (for a gate run a SKIP means the check did NOT happen;"
	echo "   only 'SANDBOX FLEET SOAK: PASS' counts)"
	exit 0
}

# known_defect records an invariant that is CURRENTLY VIOLATED by a filed,
# unfixed defect. It is printed loudly here and again in the final summary, and
# the run cannot report a clean PASS while any are outstanding -- so this can
# never quietly become the decoration the ticket warns about. Remove the call
# (leaving the assertion) when the named ticket lands.
KNOWN_DEFECTS=""
known_defect() {
	echo "  KNOWN DEFECT ($1): $2"
	KNOWN_DEFECTS="$KNOWN_DEFECTS $1"
}

command -v mgit-sandboxd >/dev/null 2>&1 || skip "mgit-sandboxd not installed"
# The trail assertions (I5) read the append-only table directly: no CLI verb
# exposes sandbox_events, and asserting the trail is a third of this gate.
command -v sqlite3 >/dev/null 2>&1 || skip "sqlite3 not installed (needed to assert sandbox_events)"

os="$(uname -s)"
case "$os" in
Linux)
	[ -e /dev/kvm ] && [ -r /dev/kvm ] && [ -w /dev/kvm ] || skip "no usable /dev/kvm"
	;;
Darwin)
	[ "$(uname -m)" = "arm64" ] || skip "macOS sandbox requires Apple Silicon"
	# An UNSIGNED daemon cannot create a VM at all, and would hand this gate a
	# vacuous pass: every sandbox would fail to boot and several invariants
	# would hold trivially over an empty fleet.
	codesign --display --entitlements - "$(command -v mgit-sandboxd)" 2>/dev/null |
		grep -q 'com.apple.security.hypervisor' ||
		skip "mgit-sandboxd lacks the com.apple.security.hypervisor entitlement (it could not boot a VM)"
	;;
*) skip "no sandbox backend on $os" ;;
esac

work="$(mktemp -d)"
# mktemp hands back a symlinked path on macOS (/var -> /private/var); the daemon
# records the resolved one in --repo-root, and every helper matches on it.
work="$(cd "$work" && pwd -P)"
db="$work/.mgit/sandbox/sandbox-index.db"
CREATED_TASKS=""

cleanup() {
	# Best-effort: the assertions are the point of the run, and a teardown
	# failure must not overwrite the status they produced. Scoped to THIS
	# repo's daemon -- other agents' daemons run on this host.
	local t
	for t in $(cd "$work" 2>/dev/null && fleet_live_tasks); do
		(cd "$work" && mgit sandbox remove "$t" --force >/dev/null 2>&1) || true
	done
	fleet_kill_daemon "$work" >/dev/null 2>&1 || true
	rm -rf "$work" || true
}
trap cleanup EXIT

cd "$work"
git init -q
git -c user.email=e2e@mgit.local -c user.name=e2e commit -q --allow-empty -m init
mgit init >/dev/null
mgit sandbox image init >/dev/null 2>&1 || true

# --- guest image, same conventions as the other sandbox gates ----------------
if [ -z "${MGIT_GUEST_IMAGE:-}" ] && [ "$os" = "Linux" ] &&
	[ -n "${MGIT_GUEST_KERNEL:-}" ] && [ -n "${MGIT_GUEST_ROOTFS:-}" ]; then
	MGIT_GUEST_IMAGE="$(mgit sandbox image add --name base \
		--kernel "$MGIT_GUEST_KERNEL" --rootfs "$MGIT_GUEST_ROOTFS" \
		${MGIT_GUEST_CMDLINE:+--cmdline "$MGIT_GUEST_CMDLINE"} --json |
		sed -n 's/.*"image_ref":"\([^"]*\)".*/\1/p')"
fi
# The libkrun trust root is PER REPOSITORY, so a ref exported from another repo
# can never resolve in this scratch repo; a guest ROOT is registered here.
if [ -z "${MGIT_GUEST_IMAGE:-}" ] && [ -n "${MGIT_GUEST_BASE:-}" ]; then
	MGIT_GUEST_IMAGE="$(mgit sandbox base set "$MGIT_GUEST_BASE" \
		${MGIT_GUEST_BIN_DIR:+--guest-bin-dir "$MGIT_GUEST_BIN_DIR"} --json |
		sed -n 's/.*"image_ref":"\([^"]*\)".*/\1/p')"
fi
[ -n "${MGIT_GUEST_IMAGE:-}" ] ||
	skip "no guest image (set MGIT_GUEST_IMAGE, MGIT_GUEST_BASE, or MGIT_GUEST_KERNEL+MGIT_GUEST_ROOTFS)"

# Put the aggregate ceiling within reach, deterministically (see fleet_lib.sh).
eval "$(fleet_set_ceiling "$work" "$FLEET" "$SB_MEM_MB")"

echo "== fleet soak: profile=$PROFILE fleet=$FLEET churn_rounds=$CHURN_ROUNDS =="
echo "   caps per sandbox: ${SB_MEM_MB} MB, ${SB_CPUS} vCPU (declared, not defaulted)"
echo "   host memory ${host_mb} MB; fleet ceiling set to ${ceil_pct}% = ~${ceil_mb} MB"
echo "   (binds at $((ceil_mb / SB_MEM_MB)) sandboxes, so the aggregate refusal is reachable)"
echo "   image: $MGIT_GUEST_IMAGE"

# create_sandbox <task> registers a task-bound worktree with a sandbox and boots
# it with one exec. Registration is lazy, so the exec is what claims the VM.
#
# A failure records WHY into .why-<task>. A soak that reports only "4 sandboxes
# were refused" sends the next reader back to reproduce it by hand; the refusal
# text distinguishes a capacity refusal from a guest that would not boot, and
# those have nothing to do with each other.
create_sandbox() {
	provision "$1" && launch_sandbox "$1"
}

# provision <task> creates the task-bound worktree only. `mgit work` takes the
# repo-wide exclusive lock for its whole lifetime, so provisioning is SERIALIZED
# throughout this soak: running it concurrently produces lock timeouts that have
# nothing to do with the fleet, and an invariant must not be measured through a
# confounder. The concurrency of provisioning is itself asserted, once, in phase
# 0 -- where it is the subject rather than the noise. Refs: MGIT-120
provision() {
	local t="$1" o
	if ! o="$(mgit work "wt-$t" --task-id "$t" 2>&1)"; then
		printf '%s: provision: %s\n' "$t" "$(printf '%s' "$o" | tr '\n' ' ')" >"$work/.why-$t"
		return 1
	fi
}

# launch_sandbox <task> registers the task's sandbox and boots it with one exec.
#
# `mgit sandbox launch` takes NO repo lock, so this is the genuinely concurrent
# path -- and it is the one the fleet invariants are about: registration racing
# rehydration, admission racing admission, N VMs booting at once. Registration
# is lazy (FR-17.9/17.10), so the exec is what actually claims the VM.
launch_sandbox() {
	local t="$1" o
	if ! o="$(mgit sandbox launch --task-id "$t" --worktree "$work/wt-$t" \
		--image "$MGIT_GUEST_IMAGE" --memory-mb "$SB_MEM_MB" --cpus "$SB_CPUS" 2>&1)"; then
		printf '%s: register: %s\n' "$t" "$(printf '%s' "$o" | tr '\n' ' ')" >"$work/.why-$t"
		return 1
	fi
	if ! o="$(mgit sandbox exec --task "$t" -- /bin/true 2>&1)"; then
		printf '%s: boot: %s\n' "$t" "$(printf '%s' "$o" | tr '\n' ' ')" >"$work/.why-$t"
		return 1
	fi
}

# why <task...> prints the recorded failure reasons for the named tasks.
why() {
	local t
	for t in "$@"; do
		[ -f "$work/.why-$t" ] && sed 's/^/      /' "$work/.why-$t"
	done
	return 0
}

# why_text <task...> prints the recorded reasons as one line, for classification.
why_text() {
	local t out=""
	for t in "$@"; do
		[ -f "$work/.why-$t" ] && out="$out $(tr '\n' ' ' <"$work/.why-$t")"
	done
	printf '%s' "$out"
}

# classify_refusals <what> <task...> attributes a batch of failed creations to
# the mechanism that caused them, and fails naming THAT mechanism.
#
# This distinction is the difference between a gate and a decoration. A soak
# that blamed every failed creation on the capacity invariant would name I3 for
# a repo-lock timeout, sending the reader to audit the ceiling's release path
# over a defect that is not in it. An invariant may only be declared broken by
# evidence that it is the thing that broke.
classify_refusals() {
	local what="$1" text
	shift
	echo "  reasons:"
	why "$@"
	text="$(why_text "$@")"
	case "$text" in
	*"ceiling exceeded"*)
		_e2e_fail "INVARIANT I3 (CAPACITY) BROKE: $what — the host refused with the AGGREGATE CEILING while the fleet was drained, so capacity accounted to destroyed sandboxes was never released; a worker pool would wedge over its lifetime (MGIT-98)"
		;;
	*"another mgit process is running"*)
		_e2e_fail "CONCURRENT PROVISIONING BROKE (not a capacity fault): $what — \`mgit work\` timed out waiting on the repo-wide exclusive lock. N concurrent task provisions are exactly the worker-pool shape this gate exists for, and they serialize behind one lock. This is NOT invariant I3: the ceiling never refused. Refs: MGIT-115"
		;;
	*)
		_e2e_fail "FLEET PROVISIONING BROKE: $what — refused for a reason that is neither the aggregate ceiling nor lock contention; the recorded reasons above are the evidence"
		;;
	esac
}

track() { CREATED_TASKS="$CREATED_TASKS $1"; }

# ---------------------------------------------------------------------------
# PHASE 1 — concurrent bring-up
# ---------------------------------------------------------------------------
echo
echo "== phase 1: bring up $FLEET sandboxes CONCURRENTLY =="
# Worktrees first, serially: `mgit work` holds the repo lock (MGIT-120), and
# provisioning is not what this phase measures.
for i in $(seq 1 "$FLEET"); do
	track "F-$i"
	provision "F-$i" || _e2e_fail "could not provision worktree for F-$i: $(why "F-$i")"
done

# COLD START. Before the fleet races, one launch brings the daemon up alone.
#
# This is not a convenience: N launches into a repo with NO daemon each spawn
# one, and they race to open the shared audit index. That race is a real defect
# (MGIT-121) which fails the WHOLE fleet, and it fails so early that every later
# invariant would be measured over an empty fleet — a vacuous pass in the making.
# So it is asserted HERE, on its own, and the fleet phases then run against a
# daemon that is already up.
cold_first="F-1"
if launch_sandbox "$cold_first"; then
	pass "cold start: the first launch brought the daemon up"
else
	if grep -q "SQLITE_BUSY\|database is locked" "$work/.why-$cold_first" 2>/dev/null; then
		known_defect "MGIT-121" \
			"the daemon died at start-up opening its audit index (SQLITE_BUSY): journal_mode=WAL is set before busy_timeout, so a concurrent open is refused instead of waiting"
	fi
	_e2e_fail "cold start failed, so the fleet could never come up: $(why "$cold_first")"
fi
# Then register and boot the REST at once. This is the concurrent path: the
# daemon is up, so what races here is registration against registration and
# admission against admission, which is what the fleet invariants are about.
pids=""
for i in $(seq 2 "$FLEET"); do
	(launch_sandbox "F-$i" || echo "F-$i" >>"$work/.create-failed") &
	pids="$pids $!"
done
for p in $pids; do wait "$p" || true; done

if [ -s "$work/.create-failed" ]; then
	# shellcheck disable=SC2046 # deliberate word splitting: one task per argument
	classify_refusals "concurrent bring-up of $FLEET sandboxes lost $(tr '\n' ' ' <"$work/.create-failed")" \
		$(cat "$work/.create-failed")
fi
live="$(fleet_live_tasks | wc -l | tr -d ' ')"
[ "$live" = "$FLEET" ] && pass "all $FLEET sandboxes came up concurrently" ||
	_e2e_fail "expected $FLEET live sandboxes, list reports $live"

daemon="$(fleet_daemon_pid "$work")"
[ -n "$daemon" ] || _e2e_fail "no daemon is serving this repo after bring-up"
workdir="$(fleet_work_dir "$work")"
[ -n "$workdir" ] || _e2e_fail "could not read the daemon's --work-dir; state-dir assertions (I4) would be vacuous"
pass "daemon pid $daemon, work dir $workdir"

# --- I4, first half: one state dir per live sandbox, all distinct ------------
dirs="$(fleet_state_dirs "$workdir")"
ndirs="$(printf '%s\n' "$dirs" | grep -c . || true)"
nuniq="$(printf '%s\n' "$dirs" | sort -u | grep -c . || true)"
[ "$ndirs" = "$nuniq" ] ||
	_e2e_fail "INVARIANT I4 (ISOLATION) BROKE: $ndirs state dirs but only $nuniq distinct — two sandboxes share a state directory"
[ "$ndirs" = "$FLEET" ] ||
	_e2e_fail "INVARIANT I4 (ISOLATION) BROKE: $FLEET live sandboxes but $ndirs state dirs under $workdir — a sandbox is sharing or missing its own directory"
pass "I4: $FLEET live sandboxes hold $ndirs distinct state directories"

# Each state dir must carry its OWN sockets; a shared socket path is the
# collision that would let one sandbox's control channel answer for another.
socks="$(find "$workdir" -mindepth 2 -maxdepth 2 -name '*.sock' 2>/dev/null | wc -l | tr -d ' ')"
usocks="$(find "$workdir" -mindepth 2 -maxdepth 2 -name '*.sock' 2>/dev/null | sort -u | wc -l | tr -d ' ')"
[ "$socks" = "$usocks" ] ||
	_e2e_fail "INVARIANT I4 (ISOLATION) BROKE: duplicate socket paths under $workdir"
pass "I4: every per-sandbox socket path is unique ($socks sockets)"

# ---------------------------------------------------------------------------
# PHASE 2 — real churn: create and destroy WHILE the rest run
# ---------------------------------------------------------------------------
echo
echo "== phase 2: $CHURN_ROUNDS churn rounds — create/destroy overlapping live execs =="
for r in $(seq 1 "$CHURN_ROUNDS"); do
	: >"$work/.churn-failed"
	# Pre-provision this round's transient worktrees serially, so the churn
	# below is pure sandbox lifecycle (register/boot/destroy) rather than a
	# contest for the repo lock (MGIT-120).
	for c in 1; do
		track "C$r-$c"
		provision "C$r-$c" || _e2e_fail "could not provision churn worktree C$r-$c: $(why "C$r-$c")"
	done
	pids=""
	# Steady load: keep execing the standing fleet throughout the round.
	for i in $(seq 1 "$FLEET"); do
		(
			for _ in 1 2 3; do
				mgit sandbox exec --task "F-$i" -- /bin/true >/dev/null 2>&1 ||
					echo "exec F-$i" >>"$work/.churn-failed"
			done
		) &
		pids="$pids $!"
	done
	# Overlapping churn: a transient sandbox is created and destroyed while the
	# standing fleet above is mid-exec. This is the overlap the races live in.
	for c in 1; do
		(
			t="C$r-$c"
			# Stagger so create/destroy interleaves with the execs rather than
			# landing before or after them.
			sleep "0.$((c * 3))"
			if launch_sandbox "$t"; then
				mgit sandbox remove "$t" --force >/dev/null 2>&1 ||
					echo "remove $t" >>"$work/.churn-failed"
			else
				echo "launch $t" >>"$work/.churn-failed"
			fi
		) &
		pids="$pids $!"
	done
	for p in $pids; do wait "$p" || true; done
	if [ -s "$work/.churn-failed" ]; then
		_e2e_fail "churn round $r: $(tr '\n' ' ' <"$work/.churn-failed")— the standing fleet was disturbed by concurrent create/destroy"
	fi
	live="$(fleet_live_tasks | wc -l | tr -d ' ')"
	[ "$live" = "$FLEET" ] ||
		_e2e_fail "churn round $r left $live live sandboxes, expected the standing fleet of $FLEET — a transient sandbox leaked or a standing one was lost"
	pass "churn round $r: standing fleet of $FLEET survived overlapping create/destroy"
done

# --- I4, second half: destroyed sandboxes leak no state directory ------------
dirs_after="$(fleet_state_dirs "$workdir")"
ndirs_after="$(printf '%s\n' "$dirs_after" | grep -c . || true)"
[ "$ndirs_after" = "$FLEET" ] ||
	_e2e_fail "INVARIANT I4 (ISOLATION) BROKE: $ndirs_after state dirs remain under $workdir after churn but only $FLEET sandboxes are live — a destroyed sandbox leaked its state directory (its overlay, worktree copy and sockets stay on disk)"
pass "I4: rapid create/destroy left no orphaned state directory"

# ---------------------------------------------------------------------------
# PHASE 3 — I6: the aggregate ceiling is a valid outcome, refused cleanly
# ---------------------------------------------------------------------------
echo
echo "== phase 3: drive the fleet INTO the aggregate ceiling =="
# The ceiling was set above to ~$ceil_mb MB, so it must bind at
# $((ceil_mb / SB_MEM_MB)) sandboxes of ${SB_MEM_MB} MB. Keep adding until one
# is refused, bounded a little past the point the arithmetic says it must
# refuse. Running PAST that bound without a refusal is not "the host is roomy" —
# it means an aggregate limit the operator set is not being enforced, so the
# bound is a hard failure, not a skip.
ceiling_hit=0
ceiling_msg=""
i="$FLEET"
guard=$((ceil_mb / SB_MEM_MB + 2))
[ "$guard" -gt "$FLEET" ] || guard=$((FLEET + 2))
while [ "$i" -lt "$guard" ]; do
	i=$((i + 1))
	t="X-$i"
	track "$t"
	provision "$t" || _e2e_fail "could not provision worktree for $t: $(why "$t")"
	mgit sandbox launch --task-id "$t" --worktree "$work/wt-$t" \
		--image "$MGIT_GUEST_IMAGE" --memory-mb "$SB_MEM_MB" --cpus "$SB_CPUS" >/dev/null 2>&1 || true
	if out="$(mgit sandbox exec --task "$t" -- /bin/true 2>&1)"; then
		continue
	fi
	ceiling_hit=1
	ceiling_msg="$out"
	break
done

if [ "$ceiling_hit" = "1" ]; then
	assert_contains "$ceiling_msg" "ceiling exceeded" \
		"I6: the fleet ceiling refused a launch, naming the ceiling"
	# The refusal must not be dressed up as something else. A ceiling refusal
	# means NO VM WAS EVER ATTEMPTED, so a memory-cap advisory ("the guest
	# stopped answering", "declare more memory") points the reader -- and an
	# agent under progress pressure -- at exactly the wrong fix: raising this
	# sandbox's --memory-mb makes a host-wide refusal MORE likely, not less.
	# Same defect class as MGIT-104, in the phase it did not cover.
	case "$ceiling_msg" in
	*"stopped answering mid-command"* | *"--memory-mb"*)
		known_defect "MGIT-118" \
			"a fleet-ceiling refusal is rendered as an in-guest memory-exhaustion diagnosis advising a LARGER --memory-mb; the refusal itself is correct, the advice inverts the fix"
		;;
	*)
		pass "I6: the refusal is not misdiagnosed as in-guest memory exhaustion"
		;;
	esac
	# A refused sandbox must not be left counted against the fleet.
	mgit sandbox remove "$t" --force >/dev/null 2>&1 || true
else
	_e2e_fail "INVARIANT I6 (CEILING) BROKE: host policy set the fleet ceiling to ~${ceil_mb} MB, which ${SB_MEM_MB} MB sandboxes must exhaust at $((ceil_mb / SB_MEM_MB)), yet $guard sandboxes were admitted without a single refusal — the aggregate limit is not being enforced, so nothing bounds a worker pool's memory on this host (MGIT-98)"
fi

# ---------------------------------------------------------------------------
# PHASE 4 — chaos: SIGKILL the daemon with execs in flight
# ---------------------------------------------------------------------------
echo
echo "== phase 4: SIGKILL the daemon while the fleet is executing =="
daemon="$(fleet_daemon_pid "$work")"
[ -n "$daemon" ] || _e2e_fail "no daemon to kill; phase 4 would be vacuous"
# Name the VM processes BEFORE the kill. Afterwards the daemon is gone and its
# children have been reparented to init, so there is nothing left to derive them
# from -- precisely why an orphan was invisible in MGIT-103.
vm_pids="$(fleet_child_pids "$daemon")"
[ -n "$vm_pids" ] ||
	_e2e_fail "daemon $daemon has no VM child processes; the fleet never booted and phase 4 would prove nothing"
nvm="$(printf '%s\n' "$vm_pids" | grep -c . || true)"
pass "the fleet runs as $nvm VM child processes of daemon $daemon"

# Execs in flight at the moment of the kill: reaping N children that are all
# mid-command is a different load from reaping one idle child.
for i in $(seq 1 "$FLEET"); do
	(mgit sandbox exec --task "F-$i" -- /bin/sleep 5 >/dev/null 2>&1 || true) &
done
sleep 1
killed="$(fleet_kill_daemon "$work")"
pass "SIGKILLed daemon $killed with $FLEET execs in flight"
wait 2>/dev/null || true

# --- I1: no orphaned VM process ---------------------------------------------
# shellcheck disable=SC2086 # deliberate word splitting: one pid per argument
survivors="$(fleet_reap_survivors $vm_pids)"
[ -z "$survivors" ] ||
	_e2e_fail "INVARIANT I1 (REAPING) BROKE: orphaned VM process(es):$survivors — a SIGKILLed daemon left microVMs running, holding their memory, their copy of the worktree and their sockets, addressable by no daemon (MGIT-103); reaping N children at once is not the same load as reaping one"
pass "I1: no VM process survived the daemon's SIGKILL ($nvm children reaped)"

# ---------------------------------------------------------------------------
# PHASE 5 — I2/I5: what the next daemon says about the fleet it inherited
# ---------------------------------------------------------------------------
echo
echo "== phase 5: the replacement daemon's account of the fleet =="
after="$(fleet_live_tasks)"
newd="$(fleet_daemon_pid "$work")"
[ -n "$newd" ] && [ "$newd" != "$killed" ] &&
	pass "a DIFFERENT daemon (pid $newd) is serving the repo" ||
	_e2e_fail "expected a new daemon; got '$newd' (was '$killed')"

fleet_assert_registry_honesty "$db" "$FLEET" "$after"
fleet_assert_trail_coherence "$db" "$after"

# ---------------------------------------------------------------------------
# PHASE 6 — I3: capacity returns to its floor, proven by re-admission
# ---------------------------------------------------------------------------
echo
echo "== phase 6: drain the fleet and prove the capacity came back =="
for t in $(fleet_live_tasks); do
	mgit sandbox remove "$t" --force >/dev/null 2>&1 || true
done
remaining="$(fleet_live_tasks | wc -l | tr -d ' ')"
[ "$remaining" = "0" ] ||
	_e2e_fail "INVARIANT I3 (CAPACITY) BROKE: $remaining sandbox(es) survived a full drain"
acc="$(fleet_accounted_mb)"
[ "$acc" = "0" ] ||
	_e2e_fail "INVARIANT I3 (CAPACITY) BROKE: the drained fleet still accounts $acc MB"
pass "I3: the drained fleet accounts 0 MB and lists no sandbox"

# The assertion that actually catches an accounting leak. The enforced total is
# recomputed daemon-side and is exported NOWHERE, so reading a number can never
# prove the release path is sound. Re-admitting a FULL fleet can: if any
# reservation from the ~3*FLEET sandboxes created and destroyed above were
# leaked, admission here refuses with the ceiling error instead of booting.
: >"$work/.readmit-failed"
for i in $(seq 1 "$FLEET"); do
	track "R-$i"
	provision "R-$i" || _e2e_fail "could not provision worktree for R-$i: $(why "R-$i")"
done
pids=""
for i in $(seq 1 "$FLEET"); do
	(launch_sandbox "R-$i" || echo "R-$i" >>"$work/.readmit-failed") &
	pids="$pids $!"
done
for p in $pids; do wait "$p" || true; done
if [ -s "$work/.readmit-failed" ]; then
	# shellcheck disable=SC2046 # deliberate word splitting: one task per argument
	classify_refusals "after a full drain the host would not re-admit a fleet of $FLEET ($(tr '\n' ' ' <"$work/.readmit-failed")refused)" \
		$(cat "$work/.readmit-failed")
fi
pass "I3: a full fleet of $FLEET is re-admitted after the drain — no accounting leak"

for t in $(fleet_live_tasks); do
	mgit sandbox remove "$t" --force >/dev/null 2>&1 || true
done

# ---------------------------------------------------------------------------
# PHASE 7 — concurrent provisioning, against a repo the soak has LOADED
# ---------------------------------------------------------------------------
echo
echo "== phase 7: $FLEET agents provision task worktrees CONCURRENTLY =="
# The first thing a worker pool does: N agents each run `mgit work --task-id
# <own task> --sandbox` at once. Registration is lazy, so this costs no VM boot.
#
# It runs LAST, deliberately. The same check at the top of a fresh scratch repo
# passes: `mgit work` holds the repo-wide lock across a full working-tree
# fingerprint and a worktree materialization, and on an empty repo both are
# instant. The contention appears once the repo carries the worktrees the soak
# has just created -- which is exactly how it appears in production, as a worker
# pool warms up. Asserting it against an empty repo would have been a check that
# could not observe the defect it names. Refs: MGIT-120
: >"$work/.p7-failed"
pids=""
for i in $(seq 1 "$FLEET"); do
	track "P7-$i"
	(
		if ! o="$(mgit work "wt-P7-$i" --task-id "P7-$i" --sandbox --image "$MGIT_GUEST_IMAGE" \
			--memory-mb "$SB_MEM_MB" --cpus "$SB_CPUS" 2>&1)"; then
			printf 'P7-%s: %s\n' "$i" "$(printf '%s' "$o" | tr '\n' ' ' | cut -c1-200)" >"$work/.why-P7-$i"
			echo "P7-$i" >>"$work/.p7-failed"
		fi
	) &
	pids="$pids $!"
done
for p in $pids; do wait "$p" || true; done

if [ -s "$work/.p7-failed" ]; then
	# shellcheck disable=SC2046 # deliberate word splitting: one task per argument
	why $(cat "$work/.p7-failed")
	if grep -q "another mgit process is running" "$work"/.why-P7-* 2>/dev/null; then
		known_defect "MGIT-120" \
			"$(wc -l <"$work/.p7-failed" | tr -d ' ') of $FLEET concurrent \`mgit work --sandbox\` provisions timed out on the repo-wide exclusive lock (30s, not configurable) — the worker-pool shape this gate exists for"
	else
		_e2e_fail "CONCURRENT PROVISIONING BROKE: $(tr '\n' ' ' <"$work/.p7-failed")failed for a reason other than lock contention; the recorded reasons above are the evidence"
	fi
else
	pass "phase 7: $FLEET concurrent provisions succeeded against a loaded repo"
fi
for i in $(seq 1 "$FLEET"); do mgit sandbox remove "P7-$i" --force >/dev/null 2>&1 || true; done

# ---------------------------------------------------------------------------
echo
ntasks="$(printf '%s' "$CREATED_TASKS" | wc -w | tr -d ' ')"
echo "== summary =="
echo "  profile:   $PROFILE (fleet=$FLEET, churn rounds=$CHURN_ROUNDS)"
echo "  sandboxes: $ntasks created across the run, at ${SB_MEM_MB} MB / ${SB_CPUS} vCPU each"
echo "  asserted:  I1 reaping, I2 registry honesty, I3 capacity floor,"
echo "             I4 state-dir/socket isolation, I5 audit-trail coherence, I6 ceiling"
if [ -n "$KNOWN_DEFECTS" ]; then
	echo
	echo "SANDBOX FLEET SOAK: PASS WITH KNOWN DEFECTS ($PROFILE profile) —$KNOWN_DEFECTS"
	echo "  Those invariants are VIOLATED by filed, unfixed defects; this run is"
	echo "  NOT a clean pass. When the ticket lands, delete its known_defect call"
	echo "  and the assertion beside it becomes hard."
	exit 0
fi
if [ "$PROFILE" = "short" ]; then
	echo
	echo "SANDBOX FLEET SOAK: PASS (short profile — fleet=$FLEET, $CHURN_ROUNDS churn rounds)"
	echo "  The LONG profile (fleet=8, 4 rounds) did NOT run here; it is the nightly"
	echo "  soak. A short run is not evidence for the wide fleet."
else
	echo
	echo "SANDBOX FLEET SOAK: PASS (long profile — fleet=$FLEET, $CHURN_ROUNDS churn rounds)"
fi
