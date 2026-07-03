#!/usr/bin/env bash
# Shared assertion helpers for the mgit install/posture e2e scripts (MGIT-48).
# These scripts validate what a REAL user gets — a binary on PATH, no repo
# checkout — so they assert on observable behavior, not just exit codes.
set -euo pipefail

_e2e_fail() {
	echo "  FAIL: $*" >&2
	exit 1
}

pass() { echo "  ok: $*"; }

# assert_contains <haystack> <needle> <what>
assert_contains() {
	case "$1" in
	*"$2"*) pass "$3" ;;
	*) _e2e_fail "$3 — expected to contain '$2', got: $1" ;;
	esac
}

# assert_not_contains <haystack> <needle> <what>
assert_not_contains() {
	case "$1" in
	*"$2"*) _e2e_fail "$3 — must NOT contain '$2', got: $1" ;;
	*) pass "$3" ;;
	esac
}

# assert_file <path> <what>
assert_file() {
	[ -f "$1" ] || _e2e_fail "$2 — expected file $1"
	pass "$2"
}

# assert_no_file <path> <what>
assert_no_file() {
	[ ! -e "$1" ] || _e2e_fail "$2 — file $1 must NOT exist"
	pass "$2"
}

# assert_ok <what> -- <cmd...> : command must succeed
assert_ok() {
	local what="$1"
	shift
	[ "$1" = "--" ] && shift
	if "$@" >/dev/null 2>&1; then pass "$what"; else _e2e_fail "$what — command failed: $*"; fi
}

# assert_fails <what> -- <cmd...> : command must fail (non-zero)
assert_fails() {
	local what="$1"
	shift
	[ "$1" = "--" ] && shift
	if "$@" >/dev/null 2>&1; then _e2e_fail "$what — command unexpectedly succeeded: $*"; else pass "$what"; fi
}

# require_mgit ensures an mgit binary is resolvable on PATH and prints its version.
require_mgit() {
	command -v mgit >/dev/null 2>&1 || _e2e_fail "mgit not on PATH — the install channel did not place it"
	echo "using mgit: $(command -v mgit) ($(mgit version 2>/dev/null | head -1))"
}
