[English](AGENTS.md) · [中文](AGENTS.zh.md)

# infra/workflow

The AI guide to developing this component. How to use it, its boundaries and contracts: BRICKKIT.md. Why it is shaped this way: `docs/design.md`. Dependencies, configuration and deployment: component.yaml.

## Code map

| Path | Owns |
|---|---|
| `backend/module/module.go` | The only entry, `New(ctx, rt)`: builds repo → service → HTTP + gRPC, starts three background loops at once (outbox pump, overdue scan, partition maintenance). Same function standalone and in a shell |
| `backend/cmd/server/main.go` | One line, `besdk.RunStandalone(module.New)` |
| `backend/cmd/migrate/main.go` | One line, `migrate.Main(migrations.FS)`: the migration container's entry (`./migrate up`) |
| `backend/internal/repo/repo.go` | The repo type and the sentinel errors (`ErrNotFound`, `ErrInvalidArgument`, `ErrNotPending`, `ErrForbidden`) |
| `backend/internal/repo/tasks.go` | Task type and constants, `CreateTask`, `GetTask`, `BatchGetTasks`, `ListTasks` (the data-scope SQL), `scanTask` |
| `backend/internal/repo/actions.go` | The four state changes through `transitionTaskTx` (approve, reject, close, cancel), `GetTaskStatus`, the action history |
| `backend/internal/repo/idempotency.go` | claim-first idempotency for the three gRPC commands: `claimIdempotency`, `replayResult`, `finalizeIdempotency` |
| `backend/internal/repo/overdue.go` | The overdue scan: claims rows with `FOR UPDATE SKIP LOCKED`, marks them, publishes `task.overdue.v1` |
| `backend/internal/service/` | Input validation, who may see a task and who may act on it (`service.go`), the error → gRPC status mapping (`status.go`) |
| `backend/internal/http/http.go` | REST routes, each registered with its permission key; `restStatus` (the one REST-specific status: `409`); `respondDetail` |
| `backend/internal/grpc/grpc.go` | `infra.workflow.v1.WorkflowService` |
| `backend/internal/partition/` | Background loop creating weekly partitions of `event_outbox` four weeks ahead |
| `migrations/` | SQL migrations, embedded by `migrations/embed.go` |
| `contracts/` | proto, OpenAPI, event schema |
| `gen/infra/workflow/` | Generated Go code: a nested Go module, tagged on its own as gen/infra/workflow/v1.x.y; never edited by hand |

| Feature | Start here | Then |
|---|---|---|
| Who sees which tasks ("my tasks", admin view) | `backend/internal/service/service.go` (`ListMyTasks`, `ListTasksAdmin`) | `backend/internal/repo/tasks.go` (`ListTasks`, `ListInput`), `backend/internal/repo/repo_test.go` |
| Who may approve or reject | `backend/internal/service/service.go` (`actorInScope`) | `backend/internal/service/service_test.go` |
| A new field on the task | `migrations/` (a new migration) | `contracts/` (append only), `buf generate`, `backend/internal/repo/tasks.go` (`taskSelectColumns`, `scanTask`), both DTOs |
| A new state change | `backend/internal/repo/actions.go` (`transitionTaskTx`) | the CHECK constraints in `migrations/`, the event in `contracts/events/` |
| A REST response with the wrong shape or status | `backend/internal/http/http.go` | `backend/internal/service/status.go`, `backend/internal/http/http_test.go` |
| Overdue reporting | `backend/internal/repo/overdue.go` | `startOverdueScan` in `backend/module/module.go` |

## Build and test

```bash
# tests run against the test database brickkit_test_db, never brickkit_db (make test-db-init at the project root)
export TEST_PG_DSN="postgres://postgres:<password>@localhost:5432/brickkit_test_db?sslmode=disable"
make test                    # every package ends in "ok"; refuses to run without TEST_PG_DSN
go test ./... -count=1 -v | grep -c -- '--- SKIP'   # 0: no test skipped
make check-version dag-check contract-check import-scan module-check   # each prints one ✓ line
make docs-check              # "0 with errors, 0 warnings"
# migrations, as the login role:
PG_HOST=localhost PG_PORT=5432 PG_DATABASE=brickkit_test_db PG_USER=infra_workflow_rw \
  PG_PASSWORD=<password> PG_SCHEMA=infra_workflow make migrate-idempotent   # ✓ 迁移幂等
```

In the BrickEnterprise project the password is `POSTGRES_PASSWORD` in the project's `.env`; `make test-db-init ID=infra/workflow` at the project root prepares the test database and runs the migration check for you. After `buf generate` (contracts changed), the contract package needs a new tag: see Pitfalls. On a real machine, from the project root: `make verify ID=infra/workflow ROUTE=/infra/workflow/tasks FOCUS=1` builds the image, starts only what this component needs, checks migration, health and the permission check, runs a focus run, and tears down.

## Design decisions

- **An inbox, not a workflow engine.** No routing rules, no business data, no calls back: the assignee comes from the caller, the display fields come in `summary`, the outcome goes out as `task.completed.v1`.
- **No dependencies, no consumed events.** Only incoming edges; `make dag-check` fails on any dependency.
- **Two data-scope dimensions, both on the assignee**: `assignee_sub` (owner) and `assignee_dept_path` (org, prefix). "My tasks" is `owner OR org`; the admin view is `org` only; the gRPC `ListTasks` is the system view, asked for explicitly with `AllDepts`. A caller with no department gets the SDK's sentinel prefix `besdk.NoDeptPath`, which matches no row, so only the owner side is left.
- **Seeing is not acting**: a manager in the department subtree may see a task; only the assignee may approve or reject it.
- **Commands are claim-first idempotent** in one table, `command_idempotency`, keyed by `idempotency_key` and checked against the command that used it first.
- **Overdue is reported, never acted on**: the scan sets `overdue_notified_at` and publishes once; the status stays `PENDING`.

## Pitfalls

| Never | Symptom | Why |
|---|---|---|
| Add a field or branch that decides something about the business ("amount above X needs a second level") | Nothing breaks; the next requirement adds another, and the inbox turns into a hidden rules engine nobody can replace | Who approves is the caller's rule; it passes the assignee in |
| Leave `ScopeOwner` or `ScopePrefix` empty on a REST list, or say "all departments" with an empty prefix | `repo.ListTasks` answers `ErrInvalidArgument` (`400`) | An empty prefix matches everything, and inside an `OR` one match-all operand makes the whole filter match all; the repository refuses it (`TestListTasks_非系统视图ScopePrefix留空报InvalidArgument`). Take both operands from `ScopeOf`; only the gRPC system view sets `AllDepts` |
| Treat an empty `dept_path` as "the root, sees everything" | A new account with no department, granted only the view key, sees every task in the company | Real paths always start with `/` (the root department too); an empty one means no department. be-sdk-go v0.5.0 turns it into the sentinel `besdk.NoDeptPath`; `TestListMyTasks_无部门的人只看到指派给自己的待办` covers it |
| Store a value that does not start with `/` in `assignee_dept_path` | Every person without a department shares the sentinel prefix and sees every task stored with it | `CreateTask` refuses it (`INVALID_ARGUMENT`); a caller passes the assignee's real path or empty, never its own `ScopeFilter.Prefix` |
| Call `besdk.ScopeOf` on a gRPC path | The call panics (`500`) | gRPC calls carry no user claims; data scopes exist only on REST |
| Let a department manager approve because `Task.InScope` is true | A manager signs off a subordinate's approval with no record that it was delegated | `InScope` is visibility; acting requires `assignee_sub` = caller (`actorInScope`) |
| Return success for an action on a task that is no longer `PENDING` | The caller believes it acted; a concurrent action silently wins | Replays are short-circuited by idempotency earlier; reaching `transitionTaskTx` on a closed task is a real conflict (`ErrNotPending`, REST `409`) |
| Map `ErrNotPending` through `service.ToStatus` alone on REST | The frontend gets `400` and shows "bad request" instead of "already handled" | The SDK maps `FailedPrecondition` to `400`; `restStatus` turns it into `Aborted` (`409`) |
| Change `status` in the overdue scan (auto-approve, auto-close) | The task drops out of everyone's pending list, and no `task.completed.v1` is sent: the business document waits forever | Overdue is a report; escalating, reminding or auto-approving is the business component's rule |
| Change a comment or option in `contracts/infra/workflow/v1/workflow.proto` without tagging the contract package | Builds pass locally (the `replace` hides it); a shell fetching `infra-workflow/v2` compiles against the old `gen/` | Any change under `gen/infra/workflow/` needs a new tag `gen/infra/workflow/v1.x.y` and the root `go.mod` requiring it |

## Before changing code

1. Is this a business rule (who approves, what happens after)? It belongs to the business component, not here.
2. Am I adding a dependency, a consumed event or a call to another component? Stop: see Design decisions.
3. A list or a detail on REST: are both scope operands taken from `ScopeOf`? Add a real-database test where someone else's task stays invisible, and one for a caller with no department.
4. Contract change? Append only: a new field, rpc, query parameter or event. Never remove or retype one.
5. Did `gen/` change? Then the contract package needs a new tag and `go.mod` must require it.
6. A new REST route is registered with `besdk.GET` / `POST` and a permission key from `assembly.yaml`.
7. New rule → write the failing test first (real database for anything SQL), then the code.
8. Bump `metadata.version` before the first change after a release; update BRICKKIT.md and `docs/design.md` in the same commit as the code.

<!-- brickkit:managed:begin lang=en -->
<!-- maintained by brickkit (init, add, remove, upgrade, skills update): edits between these markers are overwritten -->

## BrickKit

This is a BrickKit component: `component.yaml` is all the platform reads. The rules it relies on:

- `configSchema` keys are the environment variable names the code reads. Never use a reserved name: `COMPONENT_ID`, `COMPONENT_VERSION`, `PORT`, `BRICKKIT_SERVED_MEMBERS`, `BRICKKIT_SERVED_MEMBERS_CONFIG`, or any `*_ENDPOINT`.
- Dependencies are exact versions. A dependency's address arrives as `<ID>_ENDPOINT`; an optional dependency that is absent has no variable at all, so read it with a fallback.
- `/healthz` checks only this process, never a dependency. The migration command runs from the same image and must fail on an argument it does not know.
- `BRICKKIT.md` travels to every project that uses this component and is read there without the repository: keep it in step with the code, with no relative links.
- Release: raise `metadata.version`, commit, push, `brickkit release`. `brickkit lint` checks the manifest and these docs — inside a project, run in this directory, it checks only this component (`--all` for the whole project).
- The full rules are in the `brickkit-component` skill (`.claude/skills/brickkit-component/SKILL.md` at the root of the project or repository where skills are installed; `brickkit skills update` installs it); for flags ask `brickkit <command> --help`; BrickKit's own documentation is `brickkit docs`.
<!-- brickkit:managed:end -->
