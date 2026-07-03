#!/usr/bin/env bash
# Daemon-less posture e2e (MGIT-48 job 2).
#
# Simulates a machine where mgit is installed but mgit-sandboxd is NOT present.
# Asserts the honest degraded mode (MGIT-47): `mgit work` yields a usable
# worktree, the wiring is honest (no fail-closed shims, no routing hook,
# CLAUDE.md tells the truth, a parseable "Containment: none" line), `mgit run`
# fails with the actionable install pointer, and basic commands still work.
#
# Usage: daemonless_posture.sh [bindir]
#   bindir must contain mgit but MUST NOT contain mgit-sandboxd. The CI job
#   guarantees that; locally, point it at a dir with only mgit.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
. "$here/lib.sh"

if [ "${1:-}" != "" ]; then export PATH="$1:$PATH"; fi
require_mgit

# Precondition: this posture is meaningless if the daemon is present.
if command -v mgit-sandboxd >/dev/null 2>&1; then
	_e2e_fail "mgit-sandboxd is on PATH — this job must run WITHOUT the daemon"
fi
pass "mgit-sandboxd is absent (daemon-less posture)"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
cd "$work"
git init -q
git -c user.email=e2e@mgit.local -c user.name=e2e commit -q --allow-empty -m "project init"
mgit init >/dev/null

echo "== mgit work yields a usable, honest worktree =="
out="$(mgit work wt --task-id DL-1)"
assert_contains "$out" "Containment: none" "work prints a parseable open-posture status line"
[ -d wt ] || _e2e_fail "worktree dir not created"
pass "worktree directory created"

echo "== wiring is HONEST (MGIT-47), not misleading =="
assert_no_file "wt/.mgit/shims" "no fail-closed routing shims installed"
assert_no_file "wt/.claude/settings.json" "no auto-routing hook installed"
assert_no_file "wt/.envrc" "no PATH-shim direnv block installed"
md="$(cat wt/CLAUDE.md)"
assert_not_contains "$md" "routes through \`mgit run\`" "CLAUDE.md does not falsely claim routing is active"
assert_contains "$md" "mgit-sandboxd" "CLAUDE.md points at how to enable containment"

echo "== mgit run fails closed with an actionable install pointer =="
set +e
runout="$(cd wt && mgit run -- echo ok 2>&1)"
runrc=$?
set -e
[ "$runrc" -ne 0 ] || _e2e_fail "mgit run must fail (non-zero) when no daemon is present"
pass "mgit run exits non-zero without a daemon"
assert_contains "$runout" "mgit-sandboxd" "mgit run names the missing daemon"
assert_contains "$runout" "install" "mgit run gives an install pointer"

echo "== basic agent work still succeeds on the host =="
( cd wt
  printf 'hello\n' > note.txt
  mgit add . >/dev/null
  out="$(mgit commit -m 'note')"
  assert_contains "$out" "DL-1" "commit works in the degraded worktree"
  assert_ok "verify works" -- mgit verify
)

echo "DAEMONLESS POSTURE E2E: PASS"
