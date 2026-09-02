#!/usr/bin/env bash
# check-pinned-tools.sh — refuse a build tool or action resolved at a FLOATING
# ref on any path that must be reproducible (.github/, scripts/, Makefile),
# unless the site carries a declared exception with its reason.
# Refs: MGIT-180, MGIT-179, R-H298
#
#   bash scripts/ci/check-pinned-tools.sh [root]   # non-zero on a bare floating ref
#
# pin-exempt-file: this file defines the floating-ref patterns as text; nothing here is fetched
#
# THE INCIDENT. goreleaser was installed at-latest on the release path.
# v2.18.0 raised its Go floor to 1.27 while the runner was on 1.26.x with
# GOTOOLCHAIN=local, and a documentation-only PR went red the day it was
# published — an input nobody chose, changing under a build that must be
# reproducible. The pin landed (#81); this check is what notices the NEXT
# floating ref, because a sweep done once decays the moment someone adds a
# workflow step. The asymmetry that makes it worth a gate: on the release path
# the failure lands AFTER the tag is pushed, and a tag never points twice, so
# the cost of a missing check is a burned version number rather than a red run.
#
# THREE OUTCOMES per site:
#   PINNED    an exact tag (v2.17.1), an exact module version (@v0.24.0), a full
#             40-hex commit SHA, or a value carried in a variable. Not listed.
#   DECLARED  the site carries its reason on the line itself or within four
#             lines above it:
#               # pin-exempt: <why this one floats on purpose>
#             A marker with no reason declares nothing. Listed with the reason,
#             so a float is a decision someone signed, not something nobody
#             got to yet. A whole file can be declared with `pin-exempt-file:`
#             near its top (this file, and the self-test's fixtures).
#   BARE      neither. This is the defect MGIT-179 was, and it fails.
#
# WHAT COUNTS AS FLOATING:
#   - go install / go run of a module at latest, master, main or HEAD
#   - a `uses:` action whose ref is a branch or anything that is not an exact
#     tag or a full SHA. A BARE MAJOR (actions/checkout@v4) is accepted: it is
#     mutable by the ecosystem's convention, and refusing it would force full
#     SHAs on every action, which this repository has not decided to do —
#     scripts/ci/fetch-guard-actions.txt is where each action is declared.
#   - a *version key (version:, goreleaser-version:, go-version:, …) set to
#     latest, stable, main or master
#   - a release URL resolved at latest (…/releases/latest/…, …/latest/download/…)
# Comment lines (first non-blank character `#`) are skipped: a ref in a comment
# is not fetched.
#
# THE GO FLOOR, stated in every refusal so the next pin is a decision and not
# "whatever is newest": goreleaser v2.18.0 requires Go 1.27; this repository's
# runners are on Go 1.26.x with GOTOOLCHAIN=local, so v2.17.1 is the newest
# goreleaser that builds here. Bump the two together. Refs: MGIT-179
set -uo pipefail

ROOT="${1:-$(cd "$(dirname "$0")/../.." && pwd)}"
WINDOW=4 # lines above a site in which a pin-exempt declaration counts

FLOAT_GO='go[[:space:]]+(install|run)[[:space:]]+[^[:space:]]+@(latest|master|main|HEAD)([[:space:]]|$)'
USES_ANY='uses:[[:space:]]*[A-Za-z0-9_.-]+/[A-Za-z0-9_./-]+@[^[:space:]]+'
FLOAT_VER='[A-Za-z_-]*version:[[:space:]]*["'"'"']?(latest|stable|main|master)["'"'"']?[[:space:]]*(#.*)?$'
FLOAT_URL='/releases/latest/|/latest/download/'

bare=0
declared=0
rows=""
emit() { rows="${rows}$1"$'\n'; }

# uses_is_floating <line>: 0 when the action ref is neither an exact tag nor a full SHA.
uses_is_floating() {
	local ref
	ref="$(sed -E 's/.*uses:[[:space:]]*[^@]+@([^[:space:]]+).*/\1/' <<<"$1")"
	[[ "$ref" =~ ^v?[0-9]+(\.[0-9]+)*$ ]] && return 1
	[[ "$ref" =~ ^[0-9a-f]{40}$ ]] && return 1
	return 0
}

# declaration_for <file> <lineno>: prints the pin-exempt reason in the window, or nothing.
declaration_for() {
	local file=$1 n=$2 from
	from=$((n - WINDOW))
	[ $from -lt 1 ] && from=1
	sed -n "${from},${n}p" "$file" | sed -nE 's/.*pin-exempt:[[:space:]]*(.+)$/\1/p' | tail -1
}

scan_file() {
	local file=$1 rel n line reason
	rel="${file#"$ROOT"/}"
	if head -40 "$file" | grep -qE 'pin-exempt-file:[[:space:]]*[^[:space:]]'; then
		emit "DECLARED  $rel  (whole file: $(head -40 "$file" | sed -nE 's/.*pin-exempt-file:[[:space:]]*(.+)$/\1/p' | head -1))"
		declared=$((declared + 1))
		return
	fi
	while IFS= read -r hit; do
		n="${hit%%:*}"
		line="${hit#*:}"
		[[ "$line" =~ ^[[:space:]]*# ]] && continue
		if grep -qE "$USES_ANY" <<<"$line" && ! grep -qE "$FLOAT_GO|$FLOAT_VER|$FLOAT_URL" <<<"$line"; then
			uses_is_floating "$line" || continue
		fi
		reason="$(declaration_for "$file" "$n")"
		if [ -n "$reason" ]; then
			emit "DECLARED  $rel:$n  $(sed -E 's/^[[:space:]]+//' <<<"$line")  — $reason"
			declared=$((declared + 1))
		else
			emit "BARE      $rel:$n  $(sed -E 's/^[[:space:]]+//' <<<"$line")"
			bare=$((bare + 1))
		fi
	done < <(grep -nE "$FLOAT_GO|$USES_ANY|$FLOAT_VER|$FLOAT_URL" "$file" 2>/dev/null)
}

while IFS= read -r f; do
	scan_file "$f"
done < <({ find "$ROOT/.github" -type f \( -name '*.yml' -o -name '*.yaml' \) 2>/dev/null
	find "$ROOT/scripts" -type f 2>/dev/null
	[ -f "$ROOT/Makefile" ] && echo "$ROOT/Makefile"; } | sort)

echo "== pinned-tools check: $bare bare, $declared declared =="
[ -n "$rows" ] && printf '%s' "$rows"
if [ $bare -ne 0 ]; then
	cat >&2 <<'MSG'
FAIL: a build tool or action above is resolved at a floating ref on a path
      that must be reproducible. Pin it to an exact version, or declare the
      float at the site with its reason:  # pin-exempt: <why>
      Before choosing the newest version, check the tool's Go floor against
      the runner's toolchain: goreleaser v2.18.0 needs Go 1.27, the runners
      are on 1.26.x with GOTOOLCHAIN=local, so v2.17.1 is the newest that
      builds here — bump the two together. Refs: MGIT-180, MGIT-179
MSG
	exit 1
fi
exit 0
