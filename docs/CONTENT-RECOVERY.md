# Recover task content after an upgrade or a refused delivery

**finish or land in-flight tasks before upgrading to 0.7.6**.

The beta keeps audit retention separate from permission to export or apply
content. Task rows written before the local admission ledger, and bundle rows
without local admission, have ambiguous eligibility. Sandbox land, squash
`--to-git`, `mgit diff --task-id`, `mgit export --format git`, and restore refuse
these rows. Explicit-hash diff also checks both endpoints. A task whose rows
are all pending returns `no delivered content`; this is an error, not a
successful empty patch. Logs and audit records remain available for inspection.

An initial base snapshot without a task admission is not an eligible explicit-hash
endpoint either. For a normal task patch, use `mgit diff --task-id <id>`; for a
by-hash comparison, select two admitted task commits. This refusal does not
mean a healthy repository needs recovery in a new repository. Restore (file or
whole tree), cherry-pick, rollback, and projection-conflict recovery also refuse
initial or sync-base snapshots without a task row; select a locally admitted
task commit hash instead. These refusals do not authorize an unverified
projection recovery command. `mgit show <hash>`
can inspect retained changes, including pending and legacy commits, without
admitting those commits for export or apply. An unreadable diff is an error,
not successful metadata-only output.

## Preserve inspected files as a new task

Keep the original repository and its stores for audit. Inspect the files you
want to recover. Do not copy a retained guest commit or patch without inspecting
its contents. This sequence does not mark old commits as delivered or retry the
old task: it starts new, inspected host work.

1. Create a new **empty directory outside the original repository**, then change
   to that directory. Do not run these commands in an old task-bound worktree.
2. Run `mgit init` **before** copying files, so the copied files are changes to
   record rather than part of the initial base.
3. Copy only the inspected source files into the new directory, preserving their
   intended paths and modes. Do not copy `.mgit` or `.git`. Do not copy generated
   task-binding instructions from the old worktree.
4. From the new directory, run:

   ```sh
   mgit commit -a --task-id RECOVERY-1 -m 'Recover inspected files'
   mgit export RECOVERY-1 --format git
   ```

Use a fresh task ID if `RECOVERY-1` already exists. Review the exported patch
before applying it elsewhere. A linked worktree is bound to its original task;
passing a new `--task-id` there is refused. A separate initialized repository
avoids that binding and leaves the old store intact.

## Retry a native projection conflict

A land refusal can name a current, locally admitted native checkpoint for a
conflicting path. Move the host version to a backup **outside the worktree**
first, then follow the emitted restore (or symlink), mode, unstage, and land
commands. These commands use the old native checkpoint, never the pending guest
tip. Retain the backup until you have checked the delivered files. If mgit says
that the recovery checkpoint cannot be verified, it does not offer an unverified
restore command; preserve the host files and use the inspected-file sequence
above instead.
