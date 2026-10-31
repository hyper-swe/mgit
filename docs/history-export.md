# History export

`mgit export --task-id <id> --format history` writes one JSON document with a task's
steps (its micro-commits, in recorded order) and its audit records. It is a read: it
creates no commit and appends nothing to the audit trail. Use it where you need to read
history without scraping `mgit log` or `mgit audit`.

The other formats are unchanged. `--format json` is still a bare array of index rows and
`--format audit-log` a bare array of audit entries; neither carries a version. Only
`history` is versioned.

## Reading it

Read `schema_version` first. A reader that does not know the value refuses the document
and says so; it does not guess. The reader in `internal/historyexport` (`Parse`) does
this and ignores fields it does not know within the version it supports.

## Compatibility rule

- **Adding an optional field does not change `schema_version`.** A reader must ignore
  fields it does not know.
- **Removing a field, renaming it, or changing its type or meaning bumps
  `schema_version`.** So does making a field that could be absent always present with a
  different meaning.
- The current version is **1**. The golden file `internal/historyexport/testdata/history-v1.golden.json`
  pins it; a change to that file is a schema change.

## Fields (version 1)

Top level

| Field | Type | Meaning |
|---|---|---|
| `schema_version` | integer | The format version. Always first. |
| `task_id` | string | The task exported. |
| `steps` | array of step | The task's delivered micro-commits, in the order mgit recorded them. A commit that mgit retained but did not deliver is not listed. If a delivered step was built on such a commit, mgit refuses the export instead of describing that commit. |
| `audit` | array of audit record | The task's audit-trail entries, oldest first. `[]` when there are none. |

Step

| Field | Type | Meaning |
|---|---|---|
| `position` | integer | The step's place in the task, counted from 0. |
| `commit_id` | string | The micro-commit id. |
| `content_hash` | string | The commit's integrity hash. |
| `parent_id` | string, optional | The parent commit id. Absent when there is none. |
| `agent_id` | string | The agent that recorded the step. |
| `created_at` | string | When the step was recorded, UTC, RFC 3339. |
| `sandbox_id` | string, optional | The sandbox the step came from. Absent for a step recorded outside a sandbox. |
| `commit_type` | string, optional | `normal`, `rollback`, `squash`, `merge` or `system`. |
| `message` | string, optional | The commit message as recorded. A rollback's revert step is a step like any other, and its message carries the text given to `--reason`. Absent when you export with `--omit-messages`. |
| `files` | array of file | The paths the step changed compared with the previous listed step. For the first step, the comparison is with the task's fork base. `[]` when none. |

File

| Field | Type | Meaning |
|---|---|---|
| `path` | string | The path, relative to the repository root. |
| `operation` | string | `added`, `modified`, `deleted` or `renamed`. |
| `old_hash` | string, optional | The blob id before the step. |
| `new_hash` | string, optional | The blob id after the step. |
| `binary` | boolean, optional | `true` for a binary file. |

Audit record

| Field | Type | Meaning |
|---|---|---|
| `timestamp` | string | When the operation was recorded, UTC, RFC 3339. |
| `operation` | string | For example `CREATE_COMMIT`, `SQUASH`, `ROLLBACK`, `BRANCH_CREATE`, `BRANCH_MERGE`. |
| `agent_id` | string | The agent that did it. |
| `commit_id` | string, optional | The commit the operation produced or acted on. |
| `details` | string, optional | Present only for `SQUASH`, whose text mgit writes itself. |

## Leave out step messages

To export a document without any step message, add `--omit-messages`:

```
mgit export --task-id <id> --format history --omit-messages
```

Every step then has no `message` field, so neither commit messages nor rollback reasons
are in the document. Everything else is unchanged, and `schema_version` stays the same
because `message` is an optional field. The flag applies to `--format history` only; mgit
refuses it with any other format.

## What it carries and what it leaves out

The document is built from a fixed field list, so nothing is added by accident.

By default, it does carry every step's `message`. That includes the revert step a rollback records,
whose message holds the rollback reason as typed, so **a secret typed into a message or
a `--reason` is in the export**. The same text is already shown by `mgit log`, written
into the audit log, and included by `mgit squash --to-git` in the patch's commit message.
Treat the export like those: do not put credentials in a commit message or a reason, or
export with `--omit-messages`.

It does not carry commit metadata, signatures, session ids, file contents or hunks,
absolute host paths, or the `details` of an audit record other than the squash line.
`details` is absent for a rollback on purpose, so a reason is not repeated there, but the
reason is still present in the revert step's `message`. Tests check that metadata,
signature and session id are absent, and that a reason given to a real rollback appears
in a step message and nowhere else in the document.
