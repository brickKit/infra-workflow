[English](design.md) · [中文](design.zh.md)

# infra/workflow design

Conclusions only, for whoever changes this component's design. How to use it: `BRICKKIT.md`; how the code is laid out: `AGENTS.md`.

## Boundaries

This component is a task inbox: it stores tasks that need a person, lets that person act on them, and reports the outcome. Three rules shape everything in it: it contains no business rule, it never reads a business database, and it never calls a business component back.

| Not here | Owner | Why |
|---|---|---|
| Who should approve a document (amount thresholds, escalation, approval matrix) | the business component creating the task | That is a business rule. The caller computes the assignee and passes it in; this is the boundary most likely to erode, because each extra rule looks small |
| What happens after approval | the business component, listening to `task.completed.v1` | Calling it back would be a synchronous edge from infrastructure into business, and the place where the inbox starts to know business semantics |
| The data shown in a task (customer name, amount…) | the caller, as a snapshot in `summary` | Joining business tables would tie the inbox to every business schema |
| Notifying people | a notification component, listening to `task.created.v1` / `task.overdue.v1` | Registering a task must not fail because a notification channel is down or not installed |
| Compensation and retries of a failed business operation | the component that started the operation | An orchestration engine here would need to call business components back |

Two task types. `APPROVAL` asks for a decision. `EXCEPTION` asks a person to fix data: there is deliberately no "retry" button, because retrying would mean triggering the business operation again (a call back) and an operation that failed on bad data fails again unchanged. The loop is: the person follows `deep_link` to the business screen, fixes the data and resubmits there; the business component, on success, calls `CloseTask`.

Requirements that look small but would change what this component is: "amounts above X need a second level" (the business component computes the chain and creates one task per level), "delegate to a deputy while on leave" (organisation data, not here), "show the customer name in the list" (pass it in `summary`).

## Owned data

| Table | Partitioned | Notes |
|---|---|---|
| `workflow_tasks` | no | type, status, `assignee_sub`, `assignee_dept_path` (the assignee's department, not the document's), title, `summary` JSONB, source component / aggregate / ID, `deep_link`, `due_at` (nullable), `overdue_notified_at`, `version` |
| `workflow_task_actions` | no | the action history: actor, action (`APPROVED`, `REJECTED`, `CLOSED`, `CANCELLED`), comment, time; append only |
| `command_idempotency` | no | `idempotency_key` primary key, the command that used it, the resulting task ID |
| `event_outbox` | weekly, by `created_at` | the outbox the pump publishes from; partitions are created four weeks ahead by the component itself. There is no `event_inbox`: nothing is consumed |

Status: `PENDING` is the only active state; `APPROVED`, `REJECTED`, `RESOLVED` (closed by `CloseTask`), `CANCELLED` are terminal. Every transition locks the row, checks it is still `PENDING`, changes it, appends one history row and publishes in the same transaction; a task therefore has at most one history row.

`due_at` is a first-class field: without it "overdue approvals", the most common inbox request, cannot be expressed. What to do when a task is overdue is not this component's decision; it only reports it.

## Contract surface

gRPC `infra.workflow.v1.WorkflowService`, for business components:

| rpc | Kind | Notes |
|---|---|---|
| `CreateTask` | command | idempotent by `idempotency_key`; `summary_json` optional (`{}`), invalid JSON → `INVALID_ARGUMENT` |
| `CloseTask` | command | idempotent; needs `task_id` (the caller has held it since `CreateTask`); → `RESOLVED` |
| `CancelTask` | command | idempotent; needs `task_id`; → `CANCELLED` |
| `GetTaskStatus` | read | by `task_id` **or** by the `idempotency_key` of `CreateTask`; always returns the `task_id` |
| `BatchGetTasks` | read | missing IDs are left out |
| `ListTasks` | read | filters type / status / source component; cursor by task ID |

Idempotency is claim-first: `INSERT … ON CONFLICT DO NOTHING` on `command_idempotency` before the write, in the same transaction, so two concurrent requests with one key cannot both create a task ("look up, then insert" has that window). A replay of the same command returns the first result. A key already used by a different command fails with `INVALID_ARGUMENT`: short-circuiting it would hand `CloseTask` the result of `CreateTask`, and the caller would believe a still-pending task was closed.

`GetTaskStatus` by `idempotency_key` exists because a timeout is exactly the case where the caller never received the `task_id`. Not found (`TASK_STATUS_UNSPECIFIED`) and `CANCELLED` are never merged: the first means the request never arrived and may be retried, the second that retrying is wrong.

On a task that is no longer `PENDING`, every action fails (`FAILED_PRECONDITION` on gRPC): a replay never reaches the state check, so reaching it on a closed task means a wrong `task_id` or a concurrent action that won. gRPC reads are not filtered by data scope: calls between components carry no user identity, and `besdk.ScopeOf` would panic there. `ListTasks` says so explicitly (`AllDepts` in the repository), never through an empty prefix. `CreateTask` takes `assignee_dept_path` only as a path starting with `/` or empty.

REST under `/infra/workflow`, for people:

| Path | Key | Notes |
|---|---|---|
| `GET /tasks` | `infra.workflow.task.view` | "my tasks": owner OR org (see Data scopes) |
| `GET /tasks/{id}` | `infra.workflow.task.view` | `TaskDetail`: the task plus `actions` (empty array when nobody has acted); `404` both when it does not exist and when it is outside the caller's scope, with the same error text: a `403` would tell the caller the ID exists |
| `POST /tasks/{id}/approve` | `infra.workflow.task.act` | assignee in person only; answers `TaskDetail` read after the change |
| `POST /tasks/{id}/reject` | `infra.workflow.task.act` | as approve; `comment` required (`400` without) |
| `GET /admin/tasks` | `infra.workflow.admin` | org only, optional `assignee_sub` |

`CreateTask`, `CloseTask` and `CancelTask` are never on REST: a person creating a task can only mean a person acting for a business component, which bypasses its rules. Approve / reject on a task that is no longer `PENDING` answers `409`: the SDK maps `FAILED_PRECONDITION` to `400`, so the REST layer translates this one error to `ABORTED` (`409`), which tells the screen "already handled, refresh" instead of "bad request". Approve / reject answer the full `TaskDetail`, read again after the change, so the screen refreshes from the response with the new status, the new `updated_at` and the action just recorded.

Seeing is not acting. `GET /tasks/{id}` uses the same OR as the list (a manager sees subordinates' tasks); approve / reject require `assignee_sub` = caller. Delegation and adding signers are not built, so there is no way for a manager to act for someone.

## Events

Published, all through the outbox in the same transaction as the change:

| Subject | Grade | When | Payload |
|---|---|---|---|
| `infra.workflow.task.created.v1` | core | `CreateTask` | task ID, assignee, title, type, source |
| `infra.workflow.task.completed.v1` | core | approve, reject, `CloseTask` | task ID, action (`APPROVED` / `REJECTED` / `RESOLVED`), actor (empty for `CloseTask`), comment, source |
| `infra.workflow.task.cancelled.v1` | core | `CancelTask` | task ID, reason, source |
| `infra.workflow.task.overdue.v1` | peripheral | a `PENDING` task past `due_at`, once | task ID, assignee, `due_at` |

`completed` is the only way the outcome reaches the business component; a lost one leaves a document "in approval" forever with no error, which is why the three state events are core and go through the outbox. `cancelled` is a separate subject, not a `completed` variant: a business component advancing documents on `completed` must not take a cancellation for a decision.

Consumed: none. Consuming business events means mapping "this event closes that task", which is a business rule; a voided document is cancelled by its component calling `CancelTask`.

## Dependencies

None, by design. Expected but absent:

| Not a dependency | Why |
|---|---|
| any business component | only they call this one; an edge back would make cycles possible and put business meaning here |
| a notification component | events are the coupling; it may not be installed, and registering a task must not wait on it |
| an organisation master-data component | `assignee_dept_path` is a snapshot from the caller, compared by prefix; no tree is read |
| `infra/authz`, `infra/iam-casdoor` | the permission bundle and the JWKS come from `AUTHZ_BUNDLE_URL` / `IAM_JWKS_URL`: configuration, not edges |

## Place in the synchronous call graph

Only incoming edges: `erp/sales` calls `CreateTask` (optional dependency) when an order's compensation keeps failing, and the mobile BFF reads `/infra/workflow/tasks` on behalf of the user. It calls no one, so it cannot be part of a cycle. Being infrastructure that both CRM and ERP may call does not break "no synchronous edge between CRM and ERP": this component is neither.

## Partitioning and archiving

`workflow_tasks` is not partitioned: tasks arise only where a person must step in, far fewer than transactions. That assumption fails if a customer makes every document need approval; then the volume approaches the order volume, and adding a partition key is a data migration, not a small change. `event_outbox` is partitioned weekly; `partition.Start` keeps the current week plus four ahead, since a missing partition makes every outbox write fail.

The intended retention: tasks archived to `infra_workflow_archive` when terminal **and** older than twelve months (never by age alone: an old `PENDING` task is the one most in need of being seen), the history following its task, `command_idempotency` deleted after ninety days, published outbox rows cleaned after thirty days. None of this is implemented yet; nothing writes to the archive schema.

## Data scopes

Two dimensions on `workflow_tasks`, both about the **assignee**: `owner` = `assignee_sub` (equals) and `org` = `assignee_dept_path` (prefix). Visibility follows the person the task is assigned to, not the document's department; written the other way round, a manager would not see a subordinate's task on a document from another department.

- "My tasks" (`GET /tasks`): `assignee_sub = me OR assignee_dept_path LIKE my_dept_path || '%'`. The OR is in the contract. Both operands come from the caller's `ScopeOf`; leaving the prefix empty turns it into match-all and the OR into "everyone sees everything".
- Admin view (`GET /admin/tasks`): `org` only, bypassing `owner` but not `org`: the caller's department subtree, not the whole tenant.
- System view (gRPC `ListTasks`): every department, asked for with an explicit `AllDepts`. The repository refuses an empty `ScopePrefix` on any other view with `ErrInvalidArgument`: an empty prefix is a missing value, and treating it as "unrestricted" is fail-open.
- A caller with no department: authz always issues real paths starting with `/`, the root department included, so an empty `dept_path` means the person has not been placed in a department yet. be-sdk-go turns it into the sentinel prefix `besdk.NoDeptPath`, which no row starts with: that caller sees only the tasks assigned to them in "my tasks", nothing in the admin view, and `404` on anyone else's detail. Someone who must see the whole tree is placed in the root department or given the root marker `/`.
- `assignee_dept_path` on a row is the assignee's real path or empty. A task with an empty one is in no department: only the assignee sees it, a root-department manager does not. `CreateTask` refuses any other value, so a caller that passes its own scope value by mistake cannot store the sentinel, which would make every person without a department see the others' tasks.
- Tests with real rows: someone else's task in another department never appears in "my tasks" (`TestListTasks_我的待办用OR不是AND`, `TestListMyTasks_service层注入两维ScopeOf`); a caller with no department sees only their own (`TestListMyTasks_无部门的人只看到指派给自己的待办`, `TestListTasksAdmin_无部门的管理员看不到任何部门的待办`, `TestGetTaskDetail_无部门的人看别人的待办是NotFound`); the system view still sees everything (`TestListTasks_gRPC系统视图AllDepts仍看全部`); the detail answers `404` outside the scope, indistinguishable from a missing ID (`TestGetTask_范围外404与不存在无法区分`), while approve / reject on someone else's task stay `403` (`TestApprove_范围外仍是403`).

## Reference implementations

| Project | Version | Module consulted | What was borrowed | License | Usage |
|---|---|---|---|---|---|
| Odoo | — | `mail.activity` | the shape of a light inbox: polymorphic link to any record, responsible user, `date_deadline`, summary, activity type; the deadline as a first-class field | LGPL-3 | Borrowed reasoning |
| ERPNext | — | `ToDo` and `Workflow` doctypes | counter-example: `Workflow` mixes the inbox with transition rules and stores conditions as Python expressions | GPL-3 | Borrowed reasoning |
| Camunda / Flowable | — | positioning of a BPMN engine | not taken: its value is process modelling and long-transaction state, and compensation here lives with the business component that started it | Apache-2.0 | Borrowed practice |
| DingTalk / Feishu approval | — | approval forms and approver setup | in practice people use "who approves, can I add a signer, can I withdraw", rarely complex branching: the light shape is enough | proprietary | Borrowed practice |

**Deliberately avoided**: conditions stored as runtime expressions (untestable, unreadable to an AI), rendering the inbox by querying business databases, and calling the business component back after approval. Workflow engines such as Temporal were not taken either: they persist and replay long transactions across services, which would turn the inbox into an orchestration hub that must call business components.

No slot family: the boundary leaves no room for "several reasonable inbox shapes". The real variation, approval routing, lives in the business components that compute the assignee.

## Open questions

| Question | Current answer |
|---|---|
| Multi-level approval: the caller passes the whole chain, or creates one task per level? | One task per level, created by the business component after each completion; passing the chain would move "who is next" into this component |
| Should `workflow_tasks` be partitioned? | Not now; recheck against real volumes, early, because adding a partition key is a data migration |
| Delegation, adding signers, withdrawing | Not built. Delegation would only change `assignee_sub` (this component's data); who may delegate to whom is an organisation rule (not here) |
| Archiving and cleanup (see Partitioning and archiving) | Not implemented; volumes are small so far |
| "My tasks" is ordered by task ID, though an index on `(status, due_at)` exists for an overdue-first order | ID order keeps the cursor simple; ordering by `due_at` would need it in the keyset. Decide with the frontend |
