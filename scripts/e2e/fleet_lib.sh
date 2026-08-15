#!/usr/bin/env bash
# Fleet-soak helpers (MGIT-113): process, state-dir and audit-trail probes that
# the soak's invariant assertions are built from.
#
# These live apart from lib.sh because lib.sh is the shared assertion vocabulary
# every install/posture e2e depends on, and these are specific to driving a
# FLEET: they reach for daemon pids, VM child processes, per-sandbox state
# directories and the append-only trail. Keeping them here bounds the blast
# radius of a change to the soak.
#
# Every helper is read-only about other people's work: each is scoped to ONE
# repo root, because this host runs other agents' daemons and killing by name
# alone would be both flaky and destructive.
set -euo pipefail

# fleet_daemon_pid <repo-root> prints the pid of the daemon serving THAT repo,
# or nothing. Matched on the daemon's own --repo-root argument so a parallel
# agent's daemon can never be selected.
fleet_daemon_pid() {
	ps -A -o pid=,args= 2>/dev/null |
		grep '[m]git-sandboxd' | grep -F -- "--repo-root $1 " |
		awk '{print $1}' | head -1
}

# fleet_work_dir <repo-root> prints the daemon's per-repo runtime work directory
# -- the parent of every per-sandbox state directory.
#
# Read straight off the running daemon's --work-dir argument rather than
# re-deriving it. The derivation (hash of the repo root under a per-uid runtime
# base) is an implementation detail of cmd/mgit/sandbox_connect.go, and a soak
# that re-implemented it would silently assert about the wrong directory the
# day that changed -- passing while measuring nothing.
fleet_work_dir() {
	ps -A -o pid=,args= 2>/dev/null |
		grep '[m]git-sandboxd' | grep -F -- "--repo-root $1 " |
		sed -n 's/.*--work-dir \([^ ]*\).*/\1/p' | head -1
}

# fleet_child_pids <pid> prints the direct children of a pid. For a sandbox
# daemon those are its VM processes (the __krun-vm re-exec child on libkrun, the
# firecracker VMM on Linux). ps rather than pgrep -P: identical on macOS/Linux.
fleet_child_pids() {
	ps -A -o pid=,ppid= 2>/dev/null | awk -v p="$1" '$2 == p {print $1}'
}

# fleet_kill_daemon <repo-root> SIGKILLs that repo's daemon and waits for it to
# go, printing the pid it killed. Returns non-zero when no daemon was serving.
#
# SIGKILL, not SIGTERM, on purpose: a clean shutdown DRAINS, destroying the
# sandboxes in an orderly way. The failure under test is the ungraceful one --
# the process simply ceases, as an idle exit, an OOM kill or a crash leaves it.
fleet_kill_daemon() {
	local pid i
	pid="$(fleet_daemon_pid "$1")"
	[ -n "$pid" ] || return 1
	kill -9 "$pid" 2>/dev/null || true
	for i in $(seq 1 50); do
		kill -0 "$pid" 2>/dev/null || {
			echo "$pid"
			return 0
		}
		sleep 0.1
	done
	_e2e_fail "daemon $pid survived SIGKILL"
}

# fleet_events <db> <task-id> prints that task's event_type stream in append
# order, space separated -- the durable trail an auditor would read.
fleet_events() {
	sqlite3 "$1" "SELECT event_type FROM sandbox_events WHERE task_id = '$2' ORDER BY id" 2>/dev/null |
		tr '\n' ' ' | sed 's/ *$//'
}

# fleet_tasks_with_events <db> prints every task id that has any audit event.
fleet_tasks_with_events() {
	sqlite3 "$1" "SELECT DISTINCT task_id FROM sandbox_events ORDER BY task_id" 2>/dev/null
}

# fleet_live_tasks prints the task ids the CLI currently reports, one per line.
# Driven through the real `mgit sandbox list`, because what an operator can see
# is exactly what this gate is about.
fleet_live_tasks() {
	mgit sandbox list 2>/dev/null | awk 'NF && $1 != "no" {print $1}'
}

# fleet_accounted_mb prints the memory the CLI accounts to live sandboxes.
#
# HONESTY NOTE, and it bounds what may be concluded from this number. The
# aggregate the ceiling actually enforces is recomputed daemon-side from the
# BACKEND's live VMs (internal/sandboxd/ceiling.go usage()); nothing exports it.
# `sandbox list` reports the registration roster, which under lazy provisioning
# (FR-17.9/17.10) includes `created` sandboxes that hold no VM and are NOT
# counted by the ceiling. So this is an UPPER BOUND on the accounted total, not
# the accounted total -- valid for asserting the drained floor (both are 0 when
# nothing is registered), never for asserting mid-flight capacity.
fleet_accounted_mb() {
	local total
	total="$(mgit sandbox list --json 2>/dev/null |
		tr ',' '\n' | sed -n 's/.*"memory_mb":\([0-9]*\).*/\1/p' |
		awk '{s += $1} END {print s + 0}')"
	echo "${total:-0}"
}

# fleet_state_dirs <work-dir> prints the per-sandbox state directory names.
fleet_state_dirs() {
	[ -d "$1" ] || return 0
	find "$1" -mindepth 1 -maxdepth 1 -type d -exec basename {} \; 2>/dev/null | sort
}

# fleet_set_ceiling <repo-root> <fleet> <per-sandbox-mb> writes a host policy
# that puts the aggregate ceiling within reach, and prints shell assignments for
# `host_mb`, `ceil_pct` and `ceil_mb` (eval its output).
#
# WHY LOWER IT AT ALL. The stock host-wide cap is 8 concurrent sandboxes, so
# reaching it from a fleet of 4 costs four more real boots -- minutes, on a gate
# that must run every push. Lowering the MEMORY dimension (MGIT-98's actual
# subject) instead makes a genuine aggregate refusal reachable in about one
# extra boot, and it is the aggregate path being refused rather than a count.
# The count dimension cannot be used: `max_concurrent_sandboxes` in host policy
# is not wired to the ceiling at all (MGIT-119).
#
# WHY EXACTLY ONE SANDBOX OF HEADROOM. The churn phase runs a single transient
# sandbox on top of the standing fleet, so the ceiling must admit FLEET+1 or the
# soak would be refusing its own setup. One is also the most the host should be
# asked to boot beyond the fleet: boots slow sharply with width -- 16.5 s at 4
# concurrent, 41-59 s at 8, against a 30 s client deadline -- so a soak that has
# to boot its way to the ceiling starts failing on boot latency and reporting it
# as a ceiling defect.
#
# Policy is host-side, lives outside the tree guests see, and is per-repo, so a
# scratch repo's ceiling cannot affect anything else running on the machine.
fleet_set_ceiling() {
	local root="$1" fleet="$2" mem="$3" host_mb want_mb pct
	case "$(uname -s)" in
	Darwin) host_mb=$(($(sysctl -n hw.memsize) / 1024 / 1024)) ;;
	*) host_mb=$(($(awk '/MemTotal/ {print $2}' /proc/meminfo) / 1024)) ;;
	esac
	want_mb=$(((fleet + 1) * mem + mem / 2))
	# Integer percent, rounded UP, so the fleet plus its churn always fits.
	pct=$(((want_mb * 100 + host_mb - 1) / host_mb))
	[ "$pct" -ge 1 ] || pct=1
	mkdir -p "$root/.mgit/sandbox"
	printf '{"max_total_memory_percent":%d}\n' "$pct" >"$root/.mgit/sandbox/policy.json"
	printf 'host_mb=%d; ceil_pct=%d; ceil_mb=%d\n' "$host_mb" "$pct" "$((host_mb * pct / 100))"
}

# fleet_assert_registry_honesty <db> <fleet-size> <live-tasks> asserts INVARIANT
# I2 over the standing fleet: every registration is either PRESENT AND USABLE,
# or ABSENT with a terminal audit event. The third state -- present and dead --
# is the one that matters, because an agent is told it is contained while
# containment is not there (MGIT-102).
#
# A sandbox reported `running` is USED, not merely inspected. Status is a claim;
# an exec is the only evidence for it.
fleet_assert_registry_honesty() {
	local db="$1" fleet="$2" after="$3" t task st trail
	for t in $(seq 1 "$fleet"); do
		task="F-$t"
		if printf '%s\n' "$after" | grep -qx "$task"; then
			st="$(mgit sandbox status "$task" 2>&1 || true)"
			case "$st" in
			*running*)
				mgit sandbox exec --task "$task" -- /bin/echo . >/dev/null 2>&1 ||
					_e2e_fail "INVARIANT I2 (HONESTY) BROKE: $task is reported 'running' after its daemon was SIGKILLed but cannot execute — a present-and-dead registration is the worst of the three states: an agent is told it is contained, and containment is not there (MGIT-102)"
				;;
			esac
		else
			trail="$(fleet_events "$db" "$task")"
			case "$trail" in
			*killed | *destroyed | *ttl_expired | *landed) : ;;
			*)
				_e2e_fail "INVARIANT I2 (HONESTY) BROKE: $task is absent from the registry but its audit trail is '$trail' — a trail that ends at 'created' asserts a sandbox that is gone, so a reviewer cannot tell a reaped sandbox from a lost one (MGIT-102)"
				;;
			esac
		fi
	done
	pass "I2: every registration is present-and-usable or absent-with-a-terminal-event"
}

# fleet_assert_trail_coherence <db> <live-tasks> asserts INVARIANT I5 over every
# sandbox the run touched: each history starts at `created`, never continues
# past a terminal event, and ends terminal for a sandbox that is gone.
#
# The middle rule is the one concurrency breaks: events from many sandboxes are
# appended to one table at once, and a life that continues after it ended is how
# one sandbox's events end up attributed to another's.
fleet_assert_trail_coherence() {
	local db="$1" after="$2" task trail first ev seen_terminal
	local terminal_re='killed|destroyed|ttl_expired|landed'
	for task in $(fleet_tasks_with_events "$db"); do
		trail="$(fleet_events "$db" "$task")"
		first="${trail%% *}"
		[ "$first" = "created" ] ||
			_e2e_fail "INVARIANT I5 (TRAIL) BROKE: $task's history starts at '$first', not 'created' — the trail does not reconstruct a coherent life (events interleaved, or a registration was never recorded)"
		seen_terminal=""
		for ev in $trail; do
			if [ -n "$seen_terminal" ]; then
				_e2e_fail "INVARIANT I5 (TRAIL) BROKE: $task recorded '$ev' AFTER terminal '$seen_terminal' (full trail: '$trail') — the per-sandbox history is not reconstructible"
			fi
			case "$ev" in
			killed | destroyed | ttl_expired | landed) seen_terminal="$ev" ;;
			esac
		done
		if ! printf '%s\n' "$after" | grep -qx "$task"; then
			printf '%s ' "$trail" | grep -Eq "($terminal_re) *$" ||
				_e2e_fail "INVARIANT I5 (TRAIL) BROKE: $task is gone but its trail '$trail' never reached a terminal event"
		fi
	done
	pass "I5: every sandbox's audit trail reconstructs a coherent history"
}

# fleet_reap_survivors <pid...> prints any pid still alive after a grace period,
# and kills what it finds so the gate never leaves its own orphan behind.
fleet_reap_survivors() {
	local survivors="" p i
	for p in "$@"; do
		for i in $(seq 1 100); do
			kill -0 "$p" 2>/dev/null || break
			sleep 0.1
		done
		if kill -0 "$p" 2>/dev/null; then
			survivors="$survivors $p"
			kill -9 "$p" 2>/dev/null || true
		fi
	done
	echo "$survivors"
}
