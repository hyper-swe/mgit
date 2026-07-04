#!/usr/bin/env bash
# Install-channel core-loop e2e (MGIT-48 job 1).
#
# Exercises the agent working loop a real user gets from an INSTALLED mgit —
# no repo checkout, mgit resolved from PATH — asserting real behavior, not just
# exit codes: init, worktree add/list/remove, commit, log, status, verify,
# audit, and the `squash --to-git | git apply` round-trip back onto the
# project's real git.
#
# Usage: core_loop.sh [bindir]
#   bindir (optional): a directory to prepend to PATH (where an extracted
#   release archive / `go install` output placed mgit). If omitted, mgit must
#   already be on PATH.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
. "$here/lib.sh"

if [ "${1:-}" != "" ]; then export PATH="$1:$PATH"; fi
require_mgit

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
cd "$work"

echo "== project git + mgit init =="
git init -q
git -c user.email=e2e@mgit.local -c user.name=e2e commit -q --allow-empty -m "project init"
out="$(mgit init)"
assert_contains "$out" "Initialized mgit repository" "mgit init reports success"
assert_file ".mgit/index.db" "mgit init created the .mgit store"

echo "== worktree add + list =="
out="$(mgit worktree add wt --task-id E2E-1)"
assert_contains "$out" "task E2E-1" "worktree bound to task"
out="$(mgit worktree list)"
assert_contains "$out" "E2E-1" "worktree list shows the task"

echo "== commit + log + status + verify (inside the worktree) =="
( cd wt
  printf 'package main\n\nfunc main() {}\n' > main.go
  mgit add .
  out="$(mgit status)"
  assert_contains "$out" "main.go" "status shows the staged file before commit"
  out="$(mgit commit -m 'add main')"
  assert_contains "$out" "E2E-1" "commit is task-tagged"
  out="$(mgit status)"
  assert_contains "$out" "working tree clean" "status reports clean after commit"
  out="$(mgit log --oneline)"
  assert_contains "$out" "add main" "log shows the commit"
  assert_ok "verify passes" -- mgit verify
  # A second coherent step.
  printf 'package main\n\nfunc main() { println("hi") }\n' > main.go
  mgit add .
  mgit commit -m 'print hi' >/dev/null
)

echo "== squash --to-git | git apply round-trip =="
patch="$work/e2e.patch"
mgit squash --task-id E2E-1 --to-git > "$patch"
assert_contains "$(head -1 "$patch")" "From " "squash --to-git emits a git patch"
# Apply the squashed task patch back onto the project's real git tree.
git apply --check "$patch"
git apply "$patch"
assert_file "main.go" "squash patch applied onto project git (round-trip)"
assert_contains "$(cat main.go)" "println" "applied content matches the squashed work"

echo "== audit trail records the operations =="
out="$(mgit audit)"
assert_contains "$out" "E2E-1" "audit references the task"
assert_contains "$out" "SQUASH" "audit records the squash"

echo "== worktree remove =="
mgit worktree remove wt >/dev/null
out="$(mgit worktree list)"
assert_not_contains "$out" "E2E-1" "worktree removed from the registry"

echo "CORE LOOP E2E: PASS"
