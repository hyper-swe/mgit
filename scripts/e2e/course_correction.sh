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
# Semantics under test (content-restoring since MGIT-54/55):
#   - `mgit rollback` is TASK-scoped and RESTORES CONTENT: the revert commit's
#     tree and the working directory return to the pre-task state (append-only;
#     a positional commit hash resolves its owning task).
#   - `mgit restore --all --commit <hash>` recovers the WHOLE working tree to a
#     checkpoint (MGIT-55) — the mechanical "go back to the good point".
#   - `mgit cherry-pick` MATERIALIZES the picked commit's content on the new
#     line (conflict-safe) and records its provenance.
#   - `mgit checkout` remains branch-only; the fork is `checkout -b` at HEAD
#     after recovering the desired state.
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
# MGIT-54: the rollback RESTORES the pre-task state on disk (the whole task
# is reverted — the good parts come back via restore/cherry-pick below).
[ ! -f config.txt ] || _e2e_fail "config.txt must be gone after rollback (pre-task state)"
[ ! -f feature.txt ] || _e2e_fail "feature.txt must be gone after rollback (pre-task state)"
[ ! -f util.txt ] || _e2e_fail "util.txt must be gone after rollback (pre-task state)"
pass "rollback restored the pre-task working tree (content-level, MGIT-54)"
audit="$(mgit audit)"
assert_contains "$audit" "ROLLBACK" "audit records the rollback"
assert_contains "$audit" "$c2" "audit still holds the bad commit (append-only)"

echo "== fork: checkout -b opens a new line, old attempt preserved =="
out="$(mgit checkout -b "rescue/$task")"
assert_contains "$out" "Switched to branch rescue/$task" "checkout -b switched to the fork"
assert_contains "$(mgit branch)" "* rescue/$task" "fork line is the current branch"
assert_contains "$(mgit log --oneline)" "$c2" "old attempt visible from the fork line"

echo "== salvage: restore --all to the good checkpoint + cherry-pick =="
# MGIT-55: one command returns the whole tree to the good checkpoint.
out="$(mgit restore --all --commit "$c1")"
assert_contains "$out" "Restored working tree to checkpoint" "restore --all recovers the checkpoint"
assert_contains "$(cat config.txt)" "mode = safe" "restored config is the pre-mistake bytes"
assert_not_contains "$(cat config.txt)" "WRONG" "wrong content is gone from the working tree"
assert_contains "$(cat feature.txt)" "feature v1" "good feature work recovered with the checkpoint"
[ ! -f util.txt ] || _e2e_fail "util.txt postdates the checkpoint and must not be restored by it"
mgit add . >/dev/null
fix="$(short_hash "$(mgit commit --task-id "$task" -m 'course-correct: recover good checkpoint')")"
[ -n "$fix" ] || _e2e_fail "course-correction commit not created"
pass "recovered checkpoint committed on the fork ($fix)"
# MGIT-54: cherry-pick MATERIALIZES the still-good commit's content (util.txt
# was NOT on disk before this — the bytes below prove real application).
out="$(mgit cherry-pick "$c3")"
assert_contains "$out" "cherry-picked from $c3" "cherry-pick recorded the salvaged commit"
assert_contains "$(mgit log --oneline)" "cherry-pick $c3" "pick commit carries source provenance"
assert_contains "$(cat util.txt)" "util v1" "picked content MATERIALIZED on disk (MGIT-54)"

echo "== land: squash to ONE artifact; abandoned attempt stays visible =="
out="$(mgit squash --task-id "$task" --to-git --to-git-output "$work/cc.patch")"
assert_contains "$out" "Wrote git patch" "squash exported the task patch"
assert_file "$work/cc.patch" "squash artifact patch exists"
assert_contains "$(head -1 "$work/cc.patch")" "From " "patch is git format-patch shaped"
assert_contains "$(cat "$work/cc.patch")" "+mode = safe" "landed artifact carries the CORRECTED content"
assert_contains "$(cat "$work/cc.patch")" "+feature v1" "landed artifact carries the recovered feature work"
assert_contains "$(cat "$work/cc.patch")" "+util v1" "landed artifact carries the cherry-picked util"
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
