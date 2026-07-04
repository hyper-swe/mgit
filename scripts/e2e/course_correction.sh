#!/usr/bin/env bash
# Course-correction loop e2e (README: "backtrack, fork, salvage").
#
# Drives the checkpointed-substrate loop against an INSTALLED mgit (PATH-
# resolved binary, scratch project repo) and asserts real behavior — file
# contents, log output, audit entries — not just exit codes:
#
#   1. three task micro-commits, one a deliberate wrong decision
#   2. BACKTRACK: `mgit rollback` creates an append-only revert commit
#      (history grows, the bad commit stays visible in log + audit)
#   3. FORK: `mgit checkout -b` opens a new line, old attempt preserved
#   4. SALVAGE: `mgit restore --commit` pulls good content from a checkpoint
#      and `mgit cherry-pick` re-records a still-good commit with provenance
#   5. LAND: `mgit squash` produces ONE task artifact whose exported patch
#      carries the corrected content, while the abandoned attempt remains
#      in the append-only history
#
# Verified-against-code adaptations vs the README's shorthand:
#   - `mgit rollback` is TASK-scoped (service.RollbackService.RollbackTask):
#     a positional commit hash only resolves the owning task; the revert
#     commit undoes the whole task, append-only. There is no single-commit
#     revert.
#   - `mgit checkout` accepts BRANCH NAMES only (cmd/mgit/checkout.go,
#     cobra.ExactArgs(1) -> CheckoutService.Checkout), so the fork point is
#     current HEAD, not an arbitrary earlier hash.
#   - `mgit cherry-pick` records a provenance commit (message + derived task
#     ID) but does NOT materialize the source content: commit trees are built
#     from HEAD + staging (internal/store/git/commit.go buildTreeFromStaging);
#     FileDiffs are metadata. Content-level salvage is `mgit restore <file>
#     --commit <hash>`, which this script asserts byte-for-byte.
#
# Usage: course_correction.sh [bindir]
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

task="CC-7"

# short_hash <mgit commit stdout> — extracts the [abcd1234] short ID.
short_hash() { printf '%s' "$1" | sed -n 's/^\[\([0-9a-f]*\)\].*/\1/p'; }

echo "== setup: project git + mgit init + 3 micro-commits (one wrong) =="
git init -q
git -c user.email=e2e@mgit.local -c user.name=e2e commit -q --allow-empty -m "project init"
out="$(mgit init)"
assert_contains "$out" "Initialized mgit repository" "mgit init reports success"

printf 'feature v1\n' > feature.txt
printf 'mode = safe\n' > config.txt
mgit add . >/dev/null
c1="$(short_hash "$(mgit commit --task-id "$task" -m 'add feature + safe config (good)')")"

printf 'mode = WRONG\n' > config.txt
mgit add . >/dev/null
c2="$(short_hash "$(mgit commit --task-id "$task" -m 'flip config mode (WRONG DECISION)')")"

printf 'util v1\n' > util.txt
mgit add . >/dev/null
c3="$(short_hash "$(mgit commit --task-id "$task" -m 'add util helper (good)')")"

[ -n "$c1" ] && [ -n "$c2" ] && [ -n "$c3" ] || _e2e_fail "could not capture commit hashes ($c1/$c2/$c3)"
pass "3 micro-commits created ($c1 good, $c2 WRONG, $c3 good)"
assert_contains "$(cat config.txt)" "mode = WRONG" "wrong decision is in the working tree"
assert_contains "$(mgit log --task-id "$task")" "pos=2" "task index has all 3 commits"

echo "== backtrack: rollback creates an append-only REVERT commit =="
before="$(mgit log --oneline | wc -l | tr -d ' ')"
# rollback is task-scoped: the positional hash resolves the owning task and
# the revert undoes ALL of the task's commits (append-only, nothing deleted).
out="$(mgit rollback "$c2" --reason 'config decision was wrong')"
assert_contains "$out" "Revert: config decision was wrong" "rollback reports a revert commit"
after="$(mgit log --oneline | wc -l | tr -d ' ')"
[ "$after" -eq $((before + 1)) ] || _e2e_fail "history must grow by exactly 1 (before=$before after=$after)"
pass "history grew by one revert commit ($before -> $after), nothing deleted"
log="$(mgit log --oneline)"
assert_contains "$log" "$c2" "bad commit is STILL in history after rollback"
assert_contains "$log" "WRONG DECISION" "bad commit message still readable"
assert_contains "$(mgit log --task-id "$task")" "pos=3" "revert is indexed as a NEW task position"
audit="$(mgit audit)"
assert_contains "$audit" "ROLLBACK" "audit records the rollback"
assert_contains "$audit" "$c2" "audit still holds the bad commit (append-only)"

echo "== fork: checkout -b opens a new line, old attempt preserved =="
out="$(mgit checkout -b "rescue/$task")"
assert_contains "$out" "Switched to branch rescue/$task" "checkout -b switched to the fork"
assert_contains "$(mgit branch)" "* rescue/$task" "fork line is the current branch"
assert_contains "$(mgit log --oneline)" "$c2" "old attempt visible from the fork line"

echo "== salvage: restore good content from a checkpoint + cherry-pick =="
out="$(mgit restore config.txt --commit "$c1")"
assert_contains "$out" "Restored config.txt" "restore pulls the file from the good checkpoint"
assert_contains "$(cat config.txt)" "mode = safe" "restored content is the pre-mistake bytes"
assert_not_contains "$(cat config.txt)" "WRONG" "wrong content is gone from the working tree"
mgit add config.txt >/dev/null
fix="$(short_hash "$(mgit commit --task-id "$task" -m 'course-correct: restore safe config')")"
[ -n "$fix" ] || _e2e_fail "course-correction commit not created"
pass "course-correction committed on the fork ($fix)"
# cherry-pick re-records the still-good commit on the new line with derived
# task provenance (content already carried by ancestry; see header note).
out="$(mgit cherry-pick "$c3")"
assert_contains "$out" "cherry-picked from $c3" "cherry-pick recorded the salvaged commit"
assert_contains "$(mgit log --oneline)" "cherry-pick $c3" "pick commit carries source provenance"
assert_contains "$(cat util.txt)" "util v1" "salvaged util content present on the fork line"

echo "== land: squash to ONE artifact; abandoned attempt stays visible =="
out="$(mgit squash --task-id "$task" --to-git --to-git-output "$work/cc.patch")"
assert_contains "$out" "Wrote git patch" "squash exported the task patch"
assert_file "$work/cc.patch" "squash artifact patch exists"
assert_contains "$(head -1 "$work/cc.patch")" "From " "patch is git format-patch shaped"
assert_contains "$(cat "$work/cc.patch")" "+mode = safe" "landed artifact carries the CORRECTED content"
assert_not_contains "$(grep '^+mode' "$work/cc.patch")" "WRONG" "landed artifact does NOT carry the wrong content"
assert_contains "$(mgit branch)" "task/$task" "squash landed on its own task branch"
squashes="$(mgit audit --type SQUASH | wc -l | tr -d ' ')"
[ "$squashes" -eq 1 ] || _e2e_fail "expected exactly 1 SQUASH audit entry, got $squashes"
pass "exactly one squash artifact recorded"

echo "== append-only proof: full story survives the land =="
log="$(mgit log --oneline)"
assert_contains "$log" "WRONG DECISION" "abandoned attempt still in mgit log after squash"
assert_contains "$log" "Revert: config decision was wrong" "revert still in mgit log after squash"
audit="$(mgit audit)"
for h in "$c1" "$c2" "$c3" "$fix"; do
	assert_contains "$audit" "$h" "audit retains commit $h"
done
assert_contains "$audit" "ROLLBACK" "audit retains the rollback"
assert_contains "$audit" "SQUASH" "audit retains the squash"

echo "COURSE CORRECTION E2E: PASS"
