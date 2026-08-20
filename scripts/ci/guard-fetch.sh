#!/usr/bin/env bash
# guard-fetch.sh — run one third-party fetch under the three clauses that a
# week of red CI earned. Refs: MGIT-143, MGIT-119
#
#   scripts/ci/guard-fetch.sh -t <seconds> -c <restore|none:reason> [-n <attempts>] \
#     -- <command...>
#
# THE INVERSION. A fetch reachable from CI is guarded by default; a fetch
# without a guard is a DECLARED EXCEPTION whose reason is written at the call
# site. This file is the one place the three clauses live, so fixing one fixes
# every call site rather than the one someone remembered.
#
# ---------------------------------------------------------------------------
# CLAUSE 1 — RETRY ERRORS. Bounded attempts, widening backoff, every retry
# surfaced (a run that only succeeded on the third says so), exhaustion fails
# loudly and names itself an OUTAGE rather than a flake.
#   Earned by: the Homebrew bottle CDN on 2026-08-15 — `Failed to download
#   resource "virglrenderer"`, three consecutive reds, ultimately a
#   gitlab.freedesktop.org 504. And the libkrunfw kernel tarball on 2026-08-19
#   — `curl: (18) transfer closed`, twice in a row.
#
# CLAUSE 2 — TIMEOUT HANGS. A hang is NOT a failure, and retry alone catches
# neither apt incident: apt never returned an error there was anything to
# retry, it simply sat. Every fetch gets a per-ATTEMPT bound that converts a
# hang into a failure fast enough for clause 1 to act on it.
#   Earned by: `Host tooling (mke2fs ..., iptables ...)` in run 32096641338 on
#   2026-08-18 — 21592s (5h59m52s) on a step whose median is 7s, ended only by
#   the job's own six-hour ceiling. A re-run cleared it in 3m11s. Measured
#   again at 145 minutes on another run.
#
# CLAUSE 3 — RESTORE PRECONDITIONS. Each retry clears the failed attempt's
# artifact so the next attempt is genuinely fresh. This is the clause learned
# the hard way, so the guard REFUSES to run without a decision about it: pass
# `-c <command>` to restore, or `-c none:<reason>` to declare that this fetch
# leaves nothing behind and say why. There is no silent third option.
#   Earned by: the first version of the libkrunfw retry, which said in good
#   faith that "a partial tarball is removed and refetched". It is not. A
#   truncated tarball satisfies make's existence check, so all three attempts
#   re-extracted the same corrupt archive and died identically at the extract
#   step -- and then reported, in its own words, "treat this as a real outage,
#   not a flake". Loud, well-worded, and honest about the wrong thing.
#   DIAGNOSTIC HONESTY DEPENDS ON THE ATTEMPT BEING REAL, NOT JUST THE MESSAGE
#   BEING PLAIN. A retry that does not restore its preconditions is not a
#   retry; it is the same attempt repeated, costing three times as long to
#   reach the same answer while teaching readers that retries do not help.
#   Commit 4a5b082 is the correction; this flag is the generalization.
#
# ---------------------------------------------------------------------------
# CHOOSING -t, WHICH IS NOT A ROUND NUMBER YOU LIKE.
#
# A timeout that fires on a slow-but-working fetch is worse than no timeout:
# it manufactures the very red it was added to prevent. So the bound comes
# from what these steps actually take, harvested from 298 completed runs /
# 14564 step records (2026-08-10 .. 2026-08-19, `gh run view --json jobs`):
#
#   step                                          n    p50    p90   worst OK
#   Host tooling (apt, e2e sandbox-live-linux)   134     7s    11s      214s
#   Install build prerequisites (apt, container) 256    29s    34s       56s
#   Install libkrun (bottled) (brew)             138   117s   151s      192s
#   Build the PINNED libkrunfw + libkrun         195   462s   572s      905s
#   Install Rust (rustup)                        246    17s    20s       26s
#   Install Go (curl tarball)                    246     3s     3s        5s
#   Install the pinned firecracker VMM             1     1s     1s        3s
#   Build the guest image (fc kernel + rootfs)   131    11s    13s       22s
#   go install (goreleaser / govulncheck)        310     1s    41s       61s
#
# RULE: -t is 4x the slowest SUCCESSFUL observation of that call site, rounded
# up the ladder {120, 300, 600, 900, 1800}, floored at 120s and capped at
# 1800s.
#   4x, because the spread within a single healthy step is already ~30x
#   (apt: 7s median, 214s worst success). A fetch must be four times worse
#   than the worst working run ever recorded before this calls it a hang.
#   Floor 120s, because below that the runner's own cold-start jitter is
#   comparable to the measurement, and a bound that tight is the
#   fires-on-a-working-fetch failure above.
#   Cap 1800s, because <attempts> x -t must fit inside the job budget; the one
#   site that hits the cap (the libkrunfw source build, 4x905s = 3620s) is
#   dominated by a kernel COMPILE rather than by the transfer, so 1800s is
#   still 2x its worst success and ~100x the download it is really guarding.
#
# Against the incident that earned clause 2, any of these bounds turns a
# six-hour red into a failure in minutes and hands clause 1 two more attempts
# -- which the incident record says would have cleared it.
#
# ---------------------------------------------------------------------------
# WHY A SCRIPT AND NOT A COMPOSITE ACTION OR A MAKE MACRO.
#   - A composite action is reachable only from `uses:` in a workflow. Half the
#     fetches this guards are inside scripts (scripts/sandbox-image/*.sh) that
#     a developer is told to run directly; a guard they lose when they do is
#     not one mechanism, it is two.
#   - A make macro has the same problem in reverse: nothing in .github/ goes
#     through make.
#   - This file is BOTH: exec it for one line in a workflow `run:` block, or
#     source it inside a script to get the `guarded` function in-process.
#     One implementation, both callers.
#
# Exit codes: the command's own on failure, 124 on timeout (matching coreutils
# `timeout`), 2 on misuse of this guard itself.

# --- clause 2 mechanics ----------------------------------------------------
# macOS runners ship no coreutils `timeout` and no `gtimeout`, and the
# ubuntu:20.04 container jobs run as root where `timeout` exists but the
# guard must behave identically on both. So the bound is bash-native.
#
# `set -m` is what makes the kill effective rather than decorative: without
# monitor mode the backgrounded command shares this shell's process group, so
# killing it leaves apt's or curl's children running -- the step would keep
# hanging while claiming it had been killed. With monitor mode the job is its
# own process group and `kill -- -PID` reaches the whole tree.
guarded_bounded() {
	local secs="$1" marker="$2"
	shift 2
	local pid killer rc

	set -m
	{ "$@" </dev/null; } &
	pid=$!
	set +m

	{
		sleep "$secs"
		kill -0 "$pid" 2>/dev/null || exit 0
		: >"$marker"
		kill -TERM -"$pid" 2>/dev/null || kill -TERM "$pid" 2>/dev/null
		sleep 5
		kill -KILL -"$pid" 2>/dev/null || kill -KILL "$pid" 2>/dev/null
	} 2>/dev/null &
	killer=$!

	if wait "$pid" 2>/dev/null; then rc=0; else rc=$?; fi
	kill "$killer" 2>/dev/null || true
	wait "$killer" 2>/dev/null || true
	return "$rc"
}

# guarded -t SECONDS -c RESTORE [-n ATTEMPTS] [-l LABEL] -- CMD...
guarded() {
	local secs="" restore="" attempts=3 label="" saw_sep="" backoff_base=30

	while [ "$#" -gt 0 ]; do
		case "$1" in
		-t | --timeout)
			secs="${2:-}"
			shift 2
			;;
		-b | --backoff-base)
			# Backoff is attempt * base. The default 30 is what the two shipped
			# retries already used; the self-test lowers it so proving three
			# attempts does not cost 90 seconds of sleeping.
			backoff_base="${2:-}"
			shift 2
			;;
		-c | --restore)
			restore="${2:-}"
			shift 2
			;;
		-n | --attempts)
			attempts="${2:-}"
			shift 2
			;;
		-l | --label)
			label="${2:-}"
			shift 2
			;;
		--)
			saw_sep=1
			shift
			break
			;;
		*)
			echo "guard-fetch: unknown option '$1' (options must precede --)" >&2
			return 2
			;;
		esac
	done

	# Misuse is refused, never guessed at. A guard that quietly did something
	# reasonable with a missing bound would be the "default is not a diagnosis"
	# defect (MGIT-118) rebuilt here.
	[ -n "$saw_sep" ] || {
		echo "guard-fetch: missing -- before the command" >&2
		return 2
	}
	[ "$#" -gt 0 ] || {
		echo "guard-fetch: no command after --" >&2
		return 2
	}
	case "$secs" in
	'' | *[!0-9]*)
		echo "guard-fetch: -t <seconds> is required and must be an integer." >&2
		echo "  Pick it from measurement, not feel -- see the RULE at the top of" >&2
		echo "  scripts/ci/guard-fetch.sh. A bound that fires on a slow-but-working" >&2
		echo "  fetch is worse than no bound at all." >&2
		return 2
		;;
	esac
	case "$attempts" in
	'' | *[!0-9]* | 0)
		echo "guard-fetch: -n <attempts> must be a positive integer" >&2
		return 2
		;;
	esac
	case "$backoff_base" in
	'' | *[!0-9]*)
		echo "guard-fetch: -b <seconds> must be an integer" >&2
		return 2
		;;
	esac
	# CLAUSE 3, enforced structurally. Not passing -c is not "no cleanup
	# needed"; it is an undeclared assumption, which is exactly the shape of
	# the defect this guard exists to prevent.
	[ -n "$restore" ] || {
		echo "guard-fetch: -c is required -- decide what a FAILED attempt leaves behind." >&2
		echo "  -c '<command>'      run this before each retry to make the next" >&2
		echo "                      attempt genuinely fresh (rm the partial file," >&2
		echo "                      apt-get clean, discard the truncated tarball)." >&2
		echo "  -c none:'<reason>'  declare that nothing is left behind, and say" >&2
		echo "                      why, at the call site." >&2
		echo "  A retry that does not restore its preconditions is not a retry; it" >&2
		echo "  is the same attempt repeated. Refs: MGIT-143 clause 3" >&2
		return 2
	}

	[ -n "$label" ] || label="$1"

	local declared="" restore_cmd=""
	case "$restore" in
	none:*) declared="${restore#none:}" ;;
	none)
		echo "guard-fetch: -c none needs a reason: -c none:'<why nothing is left behind>'" >&2
		return 2
		;;
	*) restore_cmd="$restore" ;;
	esac

	local marker
	marker="$(mktemp -t guardfetch.XXXXXX)" || return 2
	rm -f "$marker"

	local attempt=1 rc=0 backoff
	while :; do
		if [ "$attempt" -gt 1 ]; then
			# Restore BEFORE the attempt, so attempt N is a fresh attempt and
			# not attempt N-1 wearing a costume.
			if [ -n "$restore_cmd" ]; then
				echo "guard-fetch[$label]: restoring preconditions: $restore_cmd" >&2
				# Deliberately not fatal: a restore that fails (nothing to
				# remove, a path that never existed) must not mask the fetch
				# failure that is the real subject here.
				eval "$restore_cmd" >&2 || echo "guard-fetch[$label]: restore command returned non-zero; continuing" >&2
			else
				echo "guard-fetch[$label]: no restore needed -- $declared" >&2
			fi
		fi

		rm -f "$marker"
		guarded_bounded "$secs" "$marker" "$@"
		rc=$?
		if [ -f "$marker" ]; then rc=124; fi

		if [ "$rc" -eq 0 ]; then
			# CLAUSE 1: surface the retry. A run that only succeeded on the
			# third must say so, or the third-attempt success reads in the log
			# exactly like a first-attempt one and the outage never gets
			# counted.
			if [ "$attempt" -gt 1 ]; then
				echo "::warning::guard-fetch[$label]: succeeded on attempt $attempt/$attempts -- the earlier attempts failed, this run was NOT clean"
				echo "guard-fetch[$label]: OK on attempt $attempt/$attempts" >&2
			fi
			rm -f "$marker"
			return 0
		fi

		local how="failed (exit $rc)"
		[ "$rc" -eq 124 ] && how="HUNG -- no output, no error, killed at the ${secs}s bound"

		if [ "$attempt" -ge "$attempts" ]; then
			rm -f "$marker"
			echo "::error::guard-fetch[$label]: $how on attempt $attempt/$attempts -- READ THIS AS AN OUTAGE, NOT A FLAKE"
			{
				echo "guard-fetch[$label]: exhausted $attempts attempts."
				echo "  command : $*"
				echo "  last    : $how"
				if [ "$rc" -eq 124 ]; then
					echo "  A hang, not an error. The bound is ${secs}s; see the RULE in"
					echo "  scripts/ci/guard-fetch.sh for what that number is measured against."
				fi
				if [ -n "$restore_cmd" ]; then
					echo "  Each attempt was made fresh ('$restore_cmd' ran between them),"
					echo "  so this is $attempts real attempts, not one repeated."
				else
					echo "  No restore was needed between attempts: $declared"
					echo "  If that turns out to be wrong, this message is honest about the"
					echo "  wrong thing -- fix the -c at the call site, not the wording."
				fi
				echo "  Do not re-run this without reading it. A red absorbed as noise is"
				echo "  how a real failure eventually gets waved through."
			} >&2
			return "$rc"
		fi

		backoff=$((attempt * backoff_base))
		echo "::warning::guard-fetch[$label]: $how on attempt $attempt/$attempts; retrying in ${backoff}s"
		echo "guard-fetch[$label]: $how on attempt $attempt/$attempts; retrying in ${backoff}s" >&2
		sleep "$backoff"
		attempt=$((attempt + 1))
	done
}

# Executed rather than sourced: behave as the one-line wrapper.
# ${BASH_SOURCE[0]} is unset-safe here because this file requires bash.
if [ "${BASH_SOURCE[0]}" = "$0" ]; then
	guarded "$@"
	exit $?
fi
