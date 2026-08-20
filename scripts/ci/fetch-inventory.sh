#!/usr/bin/env bash
# fetch-inventory.sh — enumerate every third-party fetch reachable from CI and
# assert that each one is either GUARDED or a DECLARED EXCEPTION carrying its
# reason. Refs: MGIT-143
#
#   bash scripts/ci/fetch-inventory.sh          # assert; non-zero on a bare fetch
#   bash scripts/ci/fetch-inventory.sh --list   # print the inventory and exit 0
#
# WHY THIS EXISTS RATHER THAN A DOCUMENT. "Every fetch is guarded" written in a
# markdown file is true on the day it is written and unfalsifiable afterwards.
# The point of the inversion is that the safe state is the DEFAULT: adding an
# unguarded `curl` to a workflow must fail here, so a fetch without a guard is
# a decision someone made and signed, not something nobody got to yet.
#
# THREE OUTCOMES per call site:
#   GUARDED   the site goes through scripts/ci/guard-fetch.sh, so it carries
#             all three clauses (retry / timeout / restore preconditions).
#   DECLARED  the site carries its reason at the call site:
#               # fetch-guard: <why this one is not guarded>
#             on the line itself or within 12 lines above it; or the whole file
#             carries one near the top:
#               # fetch-guard-file: <why every fetch in this file is exempt>
#   BARE      neither. This is the defect this ticket closes, and it fails.
#
# `uses:` STEPS ARE A SEPARATE CLASS. A third-party action downloads its own
# tooling and cannot be wrapped in a shell guard, so each DISTINCT action is
# declared once in scripts/ci/fetch-guard-actions.txt rather than at every one
# of its call sites -- actions/checkout alone appears nine times, and nine
# copies of the same comment is the ceremony that gets a guard worked around.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
MODE="${1:-assert}"
ACTIONS_FILE="$ROOT/scripts/ci/fetch-guard-actions.txt"

# Fetch-shaped commands, matched only at a COMMAND POSITION.
#
# The anchor matters more than the verb list. Without it the scan flags the
# word "wget" in a busybox applet list, "brew install" inside a step's `name:`,
# and every echo that quotes a command in its help text -- and a report that is
# mostly false positives is one nobody reads, which is how a real bare fetch
# hides in it. $PRE is "somewhere a command can start"; $CMD is the verb.
#
# `run:[[:space:]]*` is in the list because a YAML step can put its command on
# the same line -- `run: curl -fsSL ... -o /tmp/x`. The first version of this
# scanner did not have it and let exactly that form through, which is the
# defect this whole ticket is about wearing the scanner's own clothes. It is
# now one of the cases guard-fetch-selftest.sh proves.
PRE='(^|[;&|(]|\$\(|run:)[[:space:]]*(sudo[[:space:]]+)?((!|if|then|else|elif|do|while|until|time|env|exec)[[:space:]]+)*([A-Za-z_][A-Za-z0-9_]*=[^[:space:]]*[[:space:]]+)*'
CMD='(curl|wget|apt-get|apt[[:space:]]+(install|update|upgrade)|brew[[:space:]]+(install|tap|update|upgrade|fetch|tap-new)|"?\$\{?BREW\}?"?[[:space:]]+(install|tap|fetch|tap-new)|git[[:space:]]+clone|go[[:space:]]+install[[:space:]]+[^[:space:]]*@|docker[[:space:]]+(pull|create|run)|rustup|sh[[:space:]]+[^[:space:]]*rustup|pip3?[[:space:]]+install|npm[[:space:]]+(install|ci)|cargo[[:space:]]+install|gh[[:space:]]+release[[:space:]]+download|mgit[[:space:]]+sandbox[[:space:]]+base[[:space:]]+from)'
FETCH_RE="${PRE}${CMD}"

guarded=0
declared=0
bare=0
rows=""

emit() { rows="${rows}$1"$'\n'; }

scan_file() {
	local f="$1" rel
	rel="${f#"$ROOT"/}"

	# File-level declaration: covers every site in the file. Must sit in the
	# first 60 lines so it is read before anything it excuses.
	local file_reason=""
	file_reason="$(sed -n '1,60p' "$f" | sed -n 's/.*fetch-guard-file:[[:space:]]*//p' | head -1)"

	# ONE grep over the file to find the candidate lines, then per-hit work
	# only. The first version tested every line with its own `grep` subprocess:
	# correct, and 12 seconds per scan, which made the self-test that runs this
	# five times cost a minute and a half on every PR. A gate that slow is one
	# that gets disabled rather than fixed.
	local hit n line logical i prev
	while IFS= read -r hit; do
		n="${hit%%:*}"
		line="${hit#*:}"

		# Skip comments and any line that is itself a declaration.
		case "$line" in
		*fetch-guard:* | *fetch-guard-file:*) continue ;;
		esac
		case "$(printf '%s' "$line" | tr -d '[:space:]')" in '#'*) continue ;; esac

		# The logical line: join upward across backslash continuations, so a
		# guard invocation split over two lines still counts as guarding the
		# command on the second.
		logical="$line"
		i=$((n - 1))
		while [ "$i" -ge 1 ]; do
			prev="$(sed -n "${i}p" "$f")"
			case "$prev" in
			*\\) logical="$prev $logical"; i=$((i - 1)) ;;
			*) break ;;
			esac
		done

		local status reason=""
		# A guarded site names the guard directly or through the $GUARD
		# variable the scripts bind it to.
		if printf '%s' "$logical" | grep -qE 'guard-fetch\.sh|\$\{?GUARD\}?'; then
			status=GUARDED
			guarded=$((guarded + 1))
		else
			# Inline or nearby declaration, then the file-level one. The
			# 12-line window is wide enough to reach past a workflow step's
			# `- name:` / `timeout-minutes:` / `run: |` preamble, which is
			# where a YAML declaration has to sit to be read before the thing
			# it excuses.
			local from=$((n - 12)) decl_ln=""
			[ "$from" -lt 1 ] && from=1
			decl_ln="$(sed -n "${from},${n}{/fetch-guard:/=
}" "$f" | tail -1)"
			if [ -n "$decl_ln" ]; then
				# A reason usually needs more than one comment line to be
				# worth reading, so gather the marker line and the comment
				# lines that continue it. Reporting only the first line
				# would truncate exactly the exceptions that most need
				# explaining -- which is how a declaration decays back into
				# a shrug.
				reason="$(awk -v s="$decl_ln" '
					NR < s { next }
					{ t = $0; sub(/^[[:space:]]*#[[:space:]]?/, "", t) }
					NR == s { sub(/^.*fetch-guard:[[:space:]]*/, "", t); printf "%s", t; next }
					/^[[:space:]]*#/ { printf " %s", t; next }
					{ exit }
				' "$f")"
			fi
			[ -z "$reason" ] && reason="$file_reason"
			if [ -n "$reason" ]; then
				status=DECLARED
				declared=$((declared + 1))
			else
				status=BARE
				bare=$((bare + 1))
			fi
		fi

		emit "$(printf '%-8s %s:%s\t%s\t%s' "$status" "$rel" "$n" \
			"$(printf '%s' "$line" | sed 's/^[[:space:]]*//' | cut -c1-72)" "$reason")"
	done < <(grep -nE "$FETCH_RE" "$f" 2>/dev/null)
}

# --- shell-level fetch sites ------------------------------------------------
while IFS= read -r f; do
	case "$f" in
	*/scripts/ci/guard-fetch.sh | */scripts/ci/guard-fetch-selftest.sh | */scripts/ci/fetch-inventory.sh) continue ;;
	esac
	scan_file "$f"
done < <(
	find "$ROOT/.github/workflows" -name '*.yml' -type f 2>/dev/null
	find "$ROOT/scripts" -name '*.sh' -type f 2>/dev/null
)

# --- `uses:` action sites ---------------------------------------------------
actions_bare=0
actions_rows=""
if [ -f "$ACTIONS_FILE" ]; then
	while IFS= read -r hit; do
		f="${hit%%:*}"
		rest="${hit#*:}"
		n="${rest%%:*}"
		use="$(printf '%s' "${rest#*:}" | sed 's/.*uses:[[:space:]]*//; s/[[:space:]]*$//')"
		name="${use%%@*}"
		case "$name" in ./*) continue ;; esac # a workflow in this repo is not a third-party fetch
		reason="$(grep -E "^${name}[[:space:]]" "$ACTIONS_FILE" 2>/dev/null | head -1 | sed 's/^[^[:space:]]*[[:space:]]*//')"
		if [ -n "$reason" ]; then
			declared=$((declared + 1))
			actions_rows="${actions_rows}$(printf '%-8s %s:%s\t%s\t%s' DECLARED "${f#"$ROOT"/}" "$n" "uses: $use" "$reason")"$'\n'
		else
			actions_bare=$((actions_bare + 1))
			bare=$((bare + 1))
			actions_rows="${actions_rows}$(printf '%-8s %s:%s\t%s\t%s' BARE "${f#"$ROOT"/}" "$n" "uses: $use" "")"$'\n'
		fi
	done < <(grep -rn 'uses:' "$ROOT/.github/workflows" 2>/dev/null | grep -v '^[^:]*:[0-9]*:[[:space:]]*#')
fi

# --- report -----------------------------------------------------------------
echo "== CI third-party fetch inventory =="
echo "   (scripts/ci/fetch-inventory.sh — MGIT-143)"
echo
printf '%s%s' "$rows" "$actions_rows" | sort | awk -F'\t' '
  NF { printf "%s\n    %s\n", $1, $2; if ($3 != "") printf "      reason: %s\n", $3 }'
echo
echo "guarded=$guarded  declared=$declared  bare=$bare"

[ "$MODE" = "--list" ] && exit 0

if [ "$bare" -gt 0 ]; then
	echo
	echo "::error::$bare third-party fetch(es) are neither guarded nor declared"
	{
		echo "FATAL: a fetch reachable from CI carries no guard and no reason."
		echo
		echo "  Guard it -- one line, all three clauses:"
		echo "    scripts/ci/guard-fetch.sh -t <sec> -c <restore|none:reason> -- <cmd>"
		echo "  ...picking -t from measurement (see the RULE in guard-fetch.sh)."
		echo
		echo "  Or declare it, at the call site, saying why it is exempt:"
		echo "    # fetch-guard: <reason>"
		echo
		echo "  For a third-party action, add a line to scripts/ci/fetch-guard-actions.txt."
		echo
		echo "  An unguarded fetch is allowed. An unguarded fetch nobody decided on is"
		echo "  the defect: it is how four separate outages this month were read as"
		echo "  flakes. Refs: MGIT-143, MGIT-119"
	} >&2
	exit 1
fi

echo "every third-party fetch reachable from CI is guarded or declared"
