# infra/workflow

## Purpose

A light task inbox. Business components register tasks that need a person: an `APPROVAL` (decide: approve or reject) or an `EXCEPTION` (go and fix data in the business screen). This component keeps everyone's inbox and the history of what was done, and reports the outcome back by event. It never decides who should approve, never reads business data, and never calls a business component back.

**Owns**

- Tasks: type, status (`PENDING`, then one of `APPROVED`, `REJECTED`, `RESOLVED`, `CANCELLED`), assignee (`assignee_sub`) and the assignee's department path (`assignee_dept_path`), title, a display snapshot (`summary`, JSON given by the caller and passed through untouched), the source (component, aggregate, ID), a `deep_link` into the business screen, and an optional deadline `due_at`.
- The action history of each task (who did what, with which comment, when); append only.
- The events `infra.workflow.task.created.v1`, `infra.workflow.task.completed.v1`, `infra.workflow.task.cancelled.v1` and `infra.workflow.task.overdue.v1`.

**Does not own**

- Who should approve a document (amount thresholds, escalation, approval matrices): the business component that creates the task computes the assignee and passes it in.
- What happens after approval (posting, shipping, unfreezing): the business component, which listens to `infra.workflow.task.completed.v1`.
- The business data shown in a task: the caller puts what it wants displayed into `summary` when it creates the task; this component never joins or reads a business database.
- Notifying people (DingTalk, e-mail, in-app): a notification component, which listens to `infra.workflow.task.created.v1` and `infra.workflow.task.overdue.v1`.
- The organisation tree: `assignee_dept_path` is a snapshot string given by the caller and compared by prefix. It is a real path starting with `/`, or empty when the assignee has no department; such a task is visible to the assignee only.

## Before you deploy

- **PostgreSQL**: a schema `infra_workflow`, plus `infra_workflow_archive` if your schema convention creates one (this component never writes to it). A login role `infra_workflow_rw` with `USAGE` and `CREATE` on `infra_workflow`, and its password. The migration runs as this role and creates the tables, so the role owns them; the running component creates weekly partitions of `event_outbox` itself, which needs that ownership. BrickKit creates none of this; in the BrickEnterprise assembly project `make dev-env` writes the password into `.env` and `make db-init` creates the schemas, role and grants.
- **NATS** reachable at `NATS_URL`: events go through an outbox table and a background pump. The component starts without NATS reachable; the events wait in the outbox.
- **Authorization** (`infra/authz`) and **identity** (`infra/iam-casdoor`, or any IAM serving a JWKS) reachable at `AUTHZ_BUNDLE_URL` and `IAM_JWKS_URL` for the REST routes to answer anything but errors. They are configuration, not dependencies: the component starts without them. The gRPC service carries no user identity and does not need them.
- No demo data of its own: tasks are created by business components over gRPC.

## Dependencies

None, and by design there never will be: business components call this one (gRPC `CreateTask`, `CloseTask`, `CancelTask`), and it answers only with events. It consumes no events: mapping "this business event closes that task" would be a business rule living here. In the BrickEnterprise project `erp/sales` depends on it optionally, to open an `EXCEPTION` task when an order's compensation keeps failing.

The authorization bundle and the JWKS are fetched from the URLs in the configuration, not through a dependency edge. Without them every protected route fails closed: no or invalid token → `401`; `IAM_JWKS_URL` empty or unreachable → `403`; the bundle never fetched yet → `503`; a valid user without the permission key → `403`. `/healthz` stays `200` throughout: it reports only that this process is alive.

## Configuration

| Variable | Meaning |
|---|---|
| `PG_HOST` | PostgreSQL host. Usually the project's shared value (`$var:PG_HOST`). |
| `PG_PORT` | PostgreSQL port; default `5432`. |
| `PG_DATABASE` | The database holding the `infra_workflow` schema (`$var:PG_DATABASE`). |
| `PG_USER` | The login role, `infra_workflow_rw` as a literal. Inside a shell the shell logs in with its own role and switches to this one per transaction (`SET LOCAL ROLE`), so the role name must be `<PG_SCHEMA>_rw`. |
| `PG_PASSWORD` | Password of `PG_USER`. Secret: write `${INFRA_WORKFLOW_DB_PASSWORD}` (or your secret store's reference), never the value. |
| `PG_SCHEMA` | Schema of all tables, the outbox and the migration state table (`schema_migrations_infra_workflow`); default `infra_workflow`. Write the literal anyway so every component's schema is visible in one place. |
| `NATS_URL` | NATS server the outbox pump publishes to (`$var:NATS_URL`). |
| `OTEL_BASE_URL` | OpenTelemetry collector base URL; empty (the default) exports nothing. |
| `AUTHZ_BUNDLE_URL` | URL of the authorization bundle the permission check polls, for example `http://infra-authz-2-0-0:8223/authz/bundle`. Required: without it every protected route answers `503`. Keep it in step with the authz version the project runs. |
| `IAM_JWKS_URL` | URL of the JWKS used to verify user tokens locally, for example `http://infra-iam-casdoor-2-0-0:8200/.well-known/jwks.json`. Required: without it every protected route answers `403`. |

## Contracts

- `contracts/infra/workflow/v1/workflow.proto` — gRPC `infra.workflow.v1.WorkflowService`, for business components: `CreateTask`, `CloseTask` (a business component closes the task itself, typically an `EXCEPTION` once the data is fixed → `RESOLVED`), `CancelTask` (the source document was voided → `CANCELLED`). `summary_json` on `CreateTask` is optional (stored as `{}`); when given it must be valid JSON, otherwise `INVALID_ARGUMENT`. `assignee_dept_path` must start with `/` or be empty, otherwise `INVALID_ARGUMENT`: pass the assignee's real department path, never a caller's scope value. Each command takes an `idempotency_key`; a replay of the same command returns the first result, and a key already used by a different command fails with `INVALID_ARGUMENT`. `CloseTask` / `CancelTask` need the `task_id`; on a task that is no longer `PENDING` they fail with `FAILED_PRECONDITION`. Reads: `GetTaskStatus` (by `task_id` or by the `idempotency_key` of `CreateTask`, for when `CreateTask` timed out; `TASK_STATUS_UNSPECIFIED` means not found, which is different from `CANCELLED`), `BatchGetTasks` (missing IDs are left out), `ListTasks` (filter by type, status, source component; cursor paging, `page_size` 1–200, default 50). gRPC carries no user identity, so these reads are not filtered by data scope. The Go package is the separate module `github.com/brickKit/infra-workflow/gen/infra/workflow`.
- `contracts/workflow.openapi.yaml` — REST under `/infra/workflow`, for people, every route behind a permission key: `GET /tasks` (`infra.workflow.task.view`; "my tasks": assigned to me, or to anyone in my department subtree; filters `type`, `status`; `cursor`, `page_size`), `GET /tasks/{id}` (`infra.workflow.task.view`; the task plus its `actions` history; `404` outside the caller's scope, the same answer as for a task that does not exist, so a read cannot be used to probe which IDs exist; `approve`/`reject` on an existing task outside the caller's scope still answer `403`), `POST /tasks/{id}/approve` and `POST /tasks/{id}/reject` (`infra.workflow.task.act`; only the assignee in person; reject needs a `comment`; answer the task plus its history; `409` when the task is no longer `PENDING`), `GET /admin/tasks` (`infra.workflow.admin`; everything in the caller's department subtree whoever it is assigned to, optional `assignee_sub`). A caller with no department (empty `dept_path` in the token) sees only the tasks assigned to them, and nothing in the admin view; give a person a department, or the root marker `/`, before expecting more. Creating, closing and cancelling tasks are not on REST.
- `contracts/events/workflow.events.json` — events published through the outbox: `infra.workflow.task.created.v1` (a task was registered), `infra.workflow.task.completed.v1` (approved, rejected, or resolved by `CloseTask`; the only way the outcome reaches the business component), `infra.workflow.task.cancelled.v1` (`CancelTask`), `infra.workflow.task.overdue.v1` (a `PENDING` task passed its `due_at`; sent once per task, the status does not change). No events are consumed.
- `assembly.yaml` — this project's metadata: the three permission keys above, the menu entry "我的待办", the edge route `/infra/workflow/**`, the schema and role, and the data scopes `owner` = `assignee_sub` (equals) and `org` = `assignee_dept_path` (prefix) on `workflow_tasks`.

## Shell declaration

Not a shell. It can be hosted in a Go shell (in the BrickEnterprise project, `be/go-infra`) or run on its own; the code is the same either way.
