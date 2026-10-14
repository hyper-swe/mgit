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
| `steps` | array of step | The task's micro-commits, in the order mgit recorded them. `[]` when there are none. |
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
| `message` | string, optional | The commit message. |
| `files` | array of file | The paths the step changed relative to its parent. `[]` when none. |

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

## What it leaves out

The document is built from a fixed field list, so nothing is added by accident. It does
not carry commit metadata, signatures, session ids, file contents or hunks, absolute host
paths, credentials, or the free text of audit details other than the squash line. In
particular a rollback reason, which a person types, is not exported. A test checks that
none of these appear.
