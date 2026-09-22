#!/usr/bin/env bash
# release-preflight-selftest.sh — prove release-preflight.sh REFUSES each
# condition it exists for and ACCEPTS the one shape a cut may have, on
# fixture repositories it builds itself; and that a check which cannot run
# never reads as passed. Refs: MGIT-209, R-H300
#
# EVERY git command here runs inside a fixture under the scratch root, and
# nowhere else: a first draft of this file ran `git tag` and `git push` in
# the real repository because an empty path made `cd ""` a silent no-op, and
# a v9.9.9 tag reached the public remote for four minutes (R-H295's rule,
# broken by construction). Each helper therefore proves its path is a
# directory under the scratch root before it does anything, and the fixture's
# only remote is asserted to live there before anything is pushed. A guard
# that fails ABORTS the whole self-test (exit 99): a self-test that cannot
# be sure where it is has nothing to prove.
#
# Usage: bash scripts/ci/release-preflight-selftest.sh
# fetch-guard-file: every repository below is created under mktemp; nothing is fetched
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
CHECK="$HERE/release-preflight.sh"
WORK="$(mktemp -d)"
[ -n "$WORK" ] && [ -d "$WORK" ] || { echo "release-preflight self-test: no scratch root; refusing to run" >&2; exit 99; }
trap 'rm -rf "$WORK"' EXIT
export GIT_AUTHOR_NAME=selftest GIT_AUTHOR_EMAIL=selftest@example.invalid \
	GIT_COMMITTER_NAME=selftest GIT_COMMITTER_EMAIL=selftest@example.invalid \
	GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null

fails=0
ok() { echo "  PASS: $*"; }
bad() { echo "  FAIL: $*" >&2; fails=$((fails + 1)); }
abort() { echo "  ABORT: $* — the self-test will not touch a path it cannot prove is its own" >&2; exit 99; }

# under_work <path>: the path is a non-empty, existing directory under $WORK.
under_work() {
	[ -n "${1:-}" ] || return 1
	case "$1" in "$WORK"/*) [ -d "$1" ] ;; *) return 1 ;; esac
}
# in_fixture <clone> <cmd...>: run one command inside a proven fixture clone
# whose only remote is under the scratch root; abort otherwise.
in_fixture() {
	local clone=$1; shift
	under_work "$clone" || abort "not a fixture clone: '${clone:-}'"
	local url; url="$(git -C "$clone" remote get-url origin 2>/dev/null)" || abort "fixture $clone has no origin"
	case "$url" in "$WORK"/*) ;; *) abort "fixture $clone points outside the scratch root: $url" ;; esac
	(cd "$clone" && "$@")
}

good_changelog='# Changelog

## [Unreleased]

## [9.9.9] - 2026-01-01

- the fix (MGIT-1)
- the other fix (MGIT-2)

## [9.9.8] - 2025-12-01
'
# fixture <name> <changelog>: a bare "origin" with main = init + fix + changelog commits, and a clone of it.
# Prints the clone's path.
fixture() {
	local name changelog root clone
	name=$1
	changelog=$2
	[ -n "$name" ] || abort "fixture without a name"
	root="$WORK/$name"
	clone="$root/clone"
	mkdir -p "$root" || abort "cannot create $root"
	git init -q --bare "$root/origin.git" || abort "cannot init $root/origin.git"
	git clone -q "$root/origin.git" "$clone" 2>/dev/null || abort "cannot clone into $clone"
	under_work "$clone" || abort "clone missing: $clone"
	in_fixture "$clone" git switch -q -c main
	in_fixture "$clone" sh -c 'echo a >a && git add a && git commit -qm init'
	in_fixture "$clone" sh -c 'echo fix >fix && git add fix && git commit -qm "the fix"'
	printf '%s' "$changelog" >"$clone/CHANGELOG.md"
	in_fixture "$clone" sh -c 'git add CHANGELOG.md && git commit -qm changelog'
	in_fixture "$clone" git push -q origin main
	record_commit "$clone" "${3-$good_record}"
	echo "$clone"
}
fix_sha() { in_fixture "$1" git log --format=%H --grep='^the fix$' -1; }

# The guest base record every fixture carries (check 7), and a stub mgit whose
# `sandbox base resolve` answers the digest it was given (or a fixed one for a
# bare tag) — the registry is never consulted here.
good_digest="sha256:$(printf 'a%.0s' $(seq 1 64))"
good_record="$(printf '{\n  "image": "debian:12",\n  "digest": "%s"\n}\n' "$good_digest")"
MGIT_STUB="$WORK/mgit-stub"
cat >"$MGIT_STUB" <<'STUB'
#!/usr/bin/env bash
# stub mgit: only `sandbox base resolve <ref> --json` is answered
case "$*" in
*"sandbox base resolve"*)
	ref=""; for a in "$@"; do case "$a" in --*|sandbox|base|resolve) ;; *) ref="$a" ;; esac; done
	case "$ref" in *@sha256:*) d="${ref##*@}" ;; *) d="sha256:$(printf 'c%.0s' $(seq 1 64))" ;; esac
	printf '{"digest":"%s","image":"%s","ref":"%s@%s"}\n' "$d" "${ref%%@*}" "${ref%%@*}" "$d" ;;
*) exit 3 ;;
esac
STUB
chmod +x "$MGIT_STUB"
export MGIT="$MGIT_STUB"
# record_commit <clone> <record>: commit the guest base record into a fixture (an empty record means no file).
record_commit() {
	local clone=$1 record=$2
	under_work "$clone" || abort "record_commit: not a fixture: '${clone:-}'"
	[ -n "$record" ] || return 0
	mkdir -p "$clone/internal/sandboxd/guestbase"
	printf '%s' "$record" >"$clone/internal/sandboxd/guestbase/release-base.json"
	in_fixture "$clone" sh -c 'git add internal && git commit -qm "guest base record" && git push -q origin main'
}

# refuses <clone> <needle> <what> <args...>: the check must exit non-zero AND name the line.
refuses() {
	local clone=$1 needle=$2 what=$3; shift 3
	under_work "$clone" || abort "refuses: not a fixture: '${clone:-}'"
	local out rc
	out="$(cd "$clone" && bash "$CHECK" "$@" 2>&1)"; rc=$?
	if [ $rc -ne 0 ] && grep -qF -- "$needle" <<<"$out"; then ok "$what"; else
		bad "$what (rc=$rc; expected a refusal naming: $needle)"; sed 's/^/    /' <<<"$out" >&2; fi
}
accepts() {
	local clone=$1 what=$2; shift 2
	under_work "$clone" || abort "accepts: not a fixture: '${clone:-}'"
	local out rc
	out="$(cd "$clone" && bash "$CHECK" "$@" 2>&1)"; rc=$?
	if [ $rc -eq 0 ] && grep -q 'RELEASE PREFLIGHT: PASS' <<<"$out"; then ok "$what"; else
		bad "$what (rc=$rc; expected PASS)"; sed 's/^/    /' <<<"$out" >&2; fi
}

echo "== release-preflight self-test (scratch root $WORK) =="
c=$(fixture happy "$good_changelog")
accepts "$c" "the one accepted shape passes (offline, --no-ci)" 9.9.9 --require "$(fix_sha "$c")" --ticket MGIT-1 --ticket MGIT-2 --no-ci

c=$(fixture nosection "$(printf '# Changelog\n\n## [Unreleased]\n\n## [9.9.8] - 2025-12-01\n')")
refuses "$c" "no '## [9.9.9] - YYYY-MM-DD' heading" "a missing version section is refused" 9.9.9 --no-ci

c=$(fixture noticket "$(printf '# Changelog\n\n## [Unreleased]\n\n## [9.9.9] - 2026-01-01\n\n- the fix (MGIT-1)\n')")
refuses "$c" "section does not name MGIT-2" "a section missing a required ticket is refused" 9.9.9 --ticket MGIT-1 --ticket MGIT-2 --no-ci

c=$(fixture unreleased "$(printf '# Changelog\n\n## [Unreleased]\n\n- not yet shipped (MGIT-3)\n\n## [9.9.9] - 2026-01-01\n\n- the fix (MGIT-1)\n')")
refuses "$c" "[Unreleased] carries 1 non-blank line(s)" "a non-empty [Unreleased] is refused" 9.9.9 --ticket MGIT-1 --no-ci

c=$(fixture tagged "$good_changelog")
in_fixture "$c" sh -c 'git tag v9.9.9 && git push -q origin v9.9.9 && git tag -d v9.9.9 >/dev/null'
refuses "$c" "origin already has v9.9.9" "an existing tag on the remote is refused (a tag never points twice)" 9.9.9 --no-ci
in_fixture "$c" git tag -f v9.9.9 >/dev/null   # the check's own fetch may have brought the tag back; -f keeps this step idempotent
refuses "$c" "local tag v9.9.9 already exists" "an existing local tag is refused" 9.9.9 --no-ci

c=$(fixture offmain "$good_changelog")
in_fixture "$c" sh -c 'git switch -q -c side && echo s >s && git add s && git commit -qm side && git switch -q main'
refuses "$c" "is not on origin/main" "a sha that is not on main is refused" 9.9.9 side --no-ci

c=$(fixture missingfix "$good_changelog")
in_fixture "$c" sh -c 'git switch -q -c elsewhere && echo e >e && git add e && git commit -qm elsewhere && git switch -q main'
refuses "$c" "does not contain" "a sha lacking a required commit is refused" 9.9.9 --require "$(in_fixture "$c" git rev-parse elsewhere)" --no-ci

c=$(fixture nogh "$good_changelog")
GH="$WORK/no-such-gh" refuses "$c" "cannot tell" "without gh the workflow checks are a loud FAIL, never a pass" 9.9.9 --ticket MGIT-1

# 7. the guest base record (MGIT-219)
c=$(fixture norecord "$good_changelog" "")
refuses "$c" "7. no guest base record" "a sha without the guest base record is refused" 9.9.9 --no-ci
c=$(fixture badrecord "$good_changelog" '{"image":"debian:12"}')
refuses "$c" "names no image or no digest" "a record without a digest is refused" 9.9.9 --no-ci
c=$(fixture unserved "$good_changelog")
MGIT="$WORK/no-such-mgit" refuses "$c" "cannot tell" "without mgit the record cannot be resolved: a loud FAIL, never a pass" 9.9.9 --no-ci
cat >"$WORK/mgit-unserved" <<'STUB'
#!/usr/bin/env bash
printf '{"digest":"sha256:%s"}\n' "$(printf 'd%.0s' $(seq 1 64))"
STUB
chmod +x "$WORK/mgit-unserved"
MGIT="$WORK/mgit-unserved" refuses "$c" "the registry answered" "a record the registry resolves to a different digest is refused, saying what it answered" 9.9.9 --no-ci
cat >"$WORK/mgit-old" <<'STUB'
#!/usr/bin/env bash
echo 'Error: unknown command "resolve" for "mgit sandbox base"' >&2; exit 1
STUB
chmod +x "$WORK/mgit-old"
MGIT="$WORK/mgit-old" refuses "$c" "does not know" "an mgit without the resolve verb (the installed release) is named as such, not as a registry miss" 9.9.9 --no-ci
cat >"$WORK/mgit-registryfail" <<'STUB'
#!/usr/bin/env bash
echo 'Error: guest base: https://registry/v2/library/debian/manifests/sha256:... : 404 Not Found' >&2; exit 1
STUB
chmod +x "$WORK/mgit-registryfail"
MGIT="$WORK/mgit-registryfail" refuses "$c" "resolving the recorded guest base failed" "a registry error is quoted as the reason" 9.9.9 --no-ci

# the pin script writes the record from what the registry answers
c=$(fixture pin "$good_changelog")
out="$(cd "$c" && MGIT="$MGIT_STUB" bash "$(dirname "$CHECK")/../release/pin-guest-base.sh" debian:12 2>&1)"; rc=$?
if [ $rc -eq 0 ] && grep -q "record moved: was $good_digest" <<<"$out" && grep -q "sha256:$(printf 'c%.0s' $(seq 1 64))" "$c/internal/sandboxd/guestbase/release-base.json"; then
	ok "pin-guest-base.sh rewrites the record with the resolved digest and says what moved"
else bad "pin-guest-base.sh (rc=$rc)"; sed 's/^/    /' <<<"$out" >&2; fi

# The guard itself: an empty fixture path must abort, never fall through to the current directory.
if (in_fixture "" true) 2>/dev/null; then bad "an empty fixture path was accepted"; else ok "an empty fixture path aborts the helper (the v9.9.9 lesson)"; fi
if (in_fixture "$HERE" true) 2>/dev/null; then bad "a path outside the scratch root was accepted"; else ok "a path outside the scratch root aborts the helper"; fi

echo
if [ $fails -eq 0 ]; then echo "release-preflight self-test: PASS"; else echo "release-preflight self-test: $fails FAILURE(S)"; exit 1; fi
