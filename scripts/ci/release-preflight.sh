#!/usr/bin/env bash
# release-preflight.sh — the pre-tag conditions of an mgit release as one check
# anyone can run, so no cut depends on a script that lives in one session's
# scratchpad: the v0.6.6 cut script tripped three of its own defects before
# the irreversible step and was gone by the next release, and v0.6.7's checks
# were rebuilt by hand and shipped inline in a pull-request body.
# Refs: MGIT-209, MGIT-204, MGIT-210, R-H298 (a tag never points twice), R-H300
#
# Usage:
#   scripts/ci/release-preflight.sh <version> [<sha>] [--require <commit>]...
#       [--ticket <id>]... [--remote <name>] [--no-ci]
#
# <version> is bare (0.6.7); the tag checked is v<version>. <sha> defaults to
# <remote>/main after a fetch. Each check prints PASS, FAIL or INFO; a check
# that cannot run prints FAIL with "cannot tell" — never PASS. Exit 0 only when
# every check passed. --no-ci skips the two workflow checks for an offline
# read; the tag step must never pass it.
#
# The checks, in the order the release decision names them:
#   1. the sha is on <remote>/main
#   2. it contains every --require commit (the fixes the release exists for)
#   3. CHANGELOG at the sha: an empty [Unreleased], a dated `## [<version>]`
#      heading, and that section names every --ticket
#   4. no v<version> tag, locally or on the remote
#   5. ci.yml at the sha: every job that ran concluded success (job
#      conclusions, never the run's summary — a run can read queued for hours
#      over a failed job)
#   6. e2e.yml push run at the sha, or at the nearest first-parent ancestor
#      that has one (a docs-only commit skips e2e by paths-ignore), with the
#      files changed between them stated
#   7. the guest base record at the sha (internal/sandboxd/guestbase/
#      release-base.json — the base the release is smoke-tested with):
#      present, naming an image and a digest, and that digest still served
#      by the registry (`mgit sandbox base resolve <image>@<digest>`); MGIT
#      picks the binary, and a missing binary is a loud FAIL
# fetch-guard-file: this script fetches nothing; `gh` reads the API and `git fetch` updates refs
set -uo pipefail

version=""; sha=""; remote="origin"; no_ci=0
require=(); tickets=()
while [ $# -gt 0 ]; do
	case "$1" in
	--require) require+=("$2"); shift 2 ;;
	--ticket) tickets+=("$2"); shift 2 ;;
	--remote) remote="$2"; shift 2 ;;
	--no-ci) no_ci=1; shift ;;
	-h | --help) sed -n '2,30p' "$0"; exit 0 ;;
	-*) echo "release-preflight: unknown flag $1" >&2; exit 2 ;;
	*)
		if [ -z "$version" ]; then version="$1"; elif [ -z "$sha" ]; then sha="$1"; else
			echo "release-preflight: unexpected argument $1" >&2; exit 2
		fi
		shift ;;
	esac
done
[ -n "$version" ] || { echo "usage: $0 <version> [<sha>] [--require <commit>]... [--ticket <id>]... [--no-ci]" >&2; exit 2; }
tag="v$version"
GH="${GH:-gh}" # the selftest points this at a stub; a missing gh is a loud FAIL, never a pass
MGIT="${MGIT:-mgit}" # likewise for the guest base record's resolve (check 7)

fail=0
pass() { echo "  PASS $*"; }
bad() { echo "  FAIL $*"; fail=1; }
tell() { echo "  INFO $*"; }

git fetch -q "$remote" 2>/dev/null || { bad "fetch from $remote failed (cannot tell)"; echo "RELEASE PREFLIGHT: FAIL"; exit 1; }
if [ -z "$sha" ]; then sha="$(git rev-parse "$remote/main")"; fi
sha="$(git rev-parse --verify -q "$sha^{commit}" 2>/dev/null)" || { bad "1. $sha is not a commit here (cannot tell)"; echo "RELEASE PREFLIGHT: FAIL"; exit 1; }
echo "== release preflight $tag at $sha ($remote/main is $(git rev-parse --short "$remote/main")) =="

# 1. on main
if git merge-base --is-ancestor "$sha" "$remote/main"; then pass "1. ${sha:0:12} is on $remote/main"; else bad "1. ${sha:0:12} is not on $remote/main"; fi

# 2. contains the named fixes
for c in "${require[@]:-}"; do
	[ -n "$c" ] || continue
	if git merge-base --is-ancestor "$c" "$sha" 2>/dev/null; then
		pass "2. contains $c — $(git log -1 --format=%s "$c" | cut -c1-70)"
	else bad "2. does not contain $c"; fi
done

# 3. the changelog at the sha (never the working tree)
changelog="$(git show "$sha:CHANGELOG.md" 2>/dev/null)" || { changelog=""; bad "3. no CHANGELOG.md at ${sha:0:12} (cannot tell)"; }
if [ -n "$changelog" ]; then
	unrel=$(awk '/^## \[Unreleased\]/{f=1;next} /^## /{if(f)exit} f' <<<"$changelog" | grep -c -v '^[[:space:]]*$')
	if [ "$unrel" = "0" ]; then pass "3. [Unreleased] is empty"; else bad "3. [Unreleased] carries $unrel non-blank line(s)"; fi
	if grep -q "^## \[$version\] - [0-9]\{4\}-[0-9]\{2\}-[0-9]\{2\}$" <<<"$changelog"; then
		pass "3. heading: $(grep -m1 "^## \[$version\] - " <<<"$changelog")"
	else bad "3. no '## [$version] - YYYY-MM-DD' heading"; fi
	section=$(awk -v v="$version" '$0 ~ "^## \\[" v "\\] - " {f=1;next} /^## /{if(f)exit} f' <<<"$changelog")
	for t in "${tickets[@]:-}"; do
		[ -n "$t" ] || continue
		if grep -E -q "$t([^0-9.]|$)" <<<"$section"; then pass "3. section names $t"; else bad "3. section does not name $t"; fi
	done
fi

# 4. the number has never been used
if git rev-parse -q --verify "refs/tags/$tag" >/dev/null; then bad "4. local tag $tag already exists"; else pass "4. no local tag $tag"; fi
if [ -n "$(git ls-remote --tags "$remote" "refs/tags/$tag" 2>/dev/null)" ]; then bad "4. $remote already has $tag (a tag never points twice)"; else pass "4. $remote has no $tag"; fi

# jobs_green <run id> <label>: PASS only when every job that ran concluded success.
jobs_green() {
	local jobs; jobs=$("$GH" run view "$1" --json jobs --jq '.jobs[] | "\(.name)=\(.conclusion)"' 2>/dev/null) || jobs=""
	[ -n "$jobs" ] || { bad "$2 run $1: job list empty (cannot tell)"; return; }
	local notgreen skipped
	notgreen=$(grep -v '=success$' <<<"$jobs" | grep -v '=skipped$' || true)
	skipped=$(grep '=skipped$' <<<"$jobs" | sed 's/=skipped$//' | tr '\n' ',' || true)
	[ -n "$skipped" ] && tell "$2 run $1 skipped jobs: ${skipped%,}"
	if [ -z "$notgreen" ]; then pass "$2 run $1: every job that ran concluded success ($(grep -c '=success$' <<<"$jobs") jobs)"
	else bad "$2 run $1 jobs not green: $(tr '\n' ' ' <<<"$notgreen")"; fi
}
# push_run <workflow> <commit> → "id status" of the newest push run there, or nothing
push_run() { "$GH" run list --workflow "$1" --commit "$2" --event push --json databaseId,status --jq '.[0] | "\(.databaseId) \(.status)"' 2>/dev/null | grep -E '^[0-9]+ ' || true; }

if [ "$no_ci" = 1 ]; then
	tell "5./6. workflow checks skipped by --no-ci — this is not a pass; run without it before tagging"
elif ! command -v "$GH" >/dev/null 2>&1; then
	bad "5./6. $GH is not available: the workflow checks cannot run (cannot tell)"
else
	# 5. ci.yml at the sha
	run=$(push_run ci.yml "$sha")
	if [ -z "$run" ]; then bad "5. no ci.yml push run at ${sha:0:12} (cannot tell)"; else
		id=${run%% *}; st=${run##* }
		if [ "$st" = "completed" ]; then jobs_green "$id" "5. ci.yml at ${sha:0:12}"; else bad "5. ci.yml run $id is '$st', not completed"; fi
	fi
	# 6. e2e.yml at the sha or the nearest ancestor that ran it
	cur=$sha; hops=0; run=""
	while [ $hops -le 12 ]; do
		run=$(push_run e2e.yml "$cur"); [ -n "$run" ] && break
		cur=$(git rev-parse "$cur^" 2>/dev/null) || break; hops=$((hops + 1))
	done
	if [ -z "$run" ]; then bad "6. no e2e.yml push run within 12 first-parent ancestors of ${sha:0:12} (cannot tell)"; else
		id=${run%% *}; st=${run##* }
		if [ "$st" = "completed" ]; then jobs_green "$id" "6. e2e.yml push at ${cur:0:12} ($hops hop(s) back)"; else bad "6. e2e.yml run $id at ${cur:0:12} is '$st', not completed"; fi
		[ $hops -gt 0 ] && tell "6. files changed ${cur:0:12}..${sha:0:12}: $(git diff --name-only "$cur" "$sha" | tr '\n' ' ')"
	fi
fi

# 7. the guest base record at the sha, and its digest still served (MGIT-219)
record_path="internal/sandboxd/guestbase/release-base.json"
record="$(git show "$sha:$record_path" 2>/dev/null)" || record=""
if [ -z "$(tr -d '[:space:]' <<<"$record")" ]; then
	bad "7. no guest base record at ${sha:0:12} ($record_path): a release records the base it is smoke-tested with"
else
	rec_image=$(sed -n 's/.*"image": *"\([^"]*\)".*/\1/p' <<<"$record" | head -1)
	rec_digest=$(grep -o 'sha256:[0-9a-f]\{64\}' <<<"$record" | head -1 || true)
	if [ -z "$rec_image" ] || [ -z "$rec_digest" ]; then
		bad "7. the guest base record at ${sha:0:12} names no image or no digest"
	elif ! command -v "$MGIT" >/dev/null 2>&1; then
		bad "7. $MGIT is not available: the recorded guest base cannot be resolved (cannot tell)"
	else
		served=$("$MGIT" sandbox base resolve "$rec_image@$rec_digest" --json 2>/dev/null | sed -n 's/.*"digest":"\(sha256:[0-9a-f]\{64\}\)".*/\1/p' | head -1) || served=""
		if [ "$served" = "$rec_digest" ]; then pass "7. guest base record $rec_image@${rec_digest:0:19}… is still served by the registry"
		else bad "7. the registry does not serve the recorded guest base $rec_image@$rec_digest (cannot tell)"; fi
	fi
fi

echo
if [ $fail = 0 ]; then echo "RELEASE PREFLIGHT: PASS at $sha"; else echo "RELEASE PREFLIGHT: FAIL at $sha"; exit 1; fi
