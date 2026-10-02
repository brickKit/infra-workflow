[English](AGENTS.md) · [中文](AGENTS.zh.md)

# infra/workflow

给开发本组件的 AI 看的导读。怎么用、边界与契约：BRICKKIT.zh.md。为什么这样设计：`docs/design.zh.md`。依赖、配置与部署：component.yaml。

## 代码地图

| 路径 | 负责 |
|---|---|
| `backend/module/module.go` | 唯一入口 `New(ctx, rt)`：组装 repo → service → HTTP + gRPC，同时启动三个后台循环（outbox 推送、超期扫描、分区维护）。独立运行与进外壳是同一个函数 |
| `backend/cmd/server/main.go` | 一行 `besdk.RunStandalone(module.New)` |
| `backend/cmd/migrate/main.go` | 一行 `migrate.Main(migrations.FS)`：迁移容器的入口（`./migrate up`） |
| `backend/internal/repo/repo.go` | repo 类型与哨兵错误（`ErrNotFound`、`ErrInvalidArgument`、`ErrNotPending`、`ErrForbidden`） |
| `backend/internal/repo/tasks.go` | 待办类型与常量、`CreateTask`、`GetTask`、`BatchGetTasks`、`ListTasks`（数据范围的 SQL）、`scanTask` |
| `backend/internal/repo/actions.go` | 经 `transitionTaskTx` 的四种状态变化（同意、驳回、关闭、作废）、`GetTaskStatus`、处理历史 |
| `backend/internal/repo/idempotency.go` | 三个 gRPC 命令的 claim-first 幂等：`claimIdempotency`、`replayResult`、`finalizeIdempotency` |
| `backend/internal/repo/overdue.go` | 超期扫描：用 `FOR UPDATE SKIP LOCKED` 认领、打标记、发 `task.overdue.v1` |
| `backend/internal/service/` | 入参校验、谁看得见 / 谁能处理（`service.go`）、错误到 gRPC 状态码的映射（`status.go`） |
| `backend/internal/http/http.go` | REST 路由，每条都带权限键注册；`restStatus`（REST 独有的一个状态码：`409`）；`respondDetail` |
| `backend/internal/grpc/grpc.go` | `infra.workflow.v1.WorkflowService` |
| `backend/internal/partition/` | 后台循环：提前四周建好 `event_outbox` 的周分区 |
| `migrations/` | SQL 迁移，由 `migrations/embed.go` 嵌进二进制 |
| `contracts/` | proto、OpenAPI、事件 schema |
| `gen/infra/workflow/` | 生成的 Go 代码：嵌套的独立 Go 模块，单独打 tag（gen/infra/workflow/v1.x.y）；从不手改 |

| 功能 | 从这里看起 | 然后 |
|---|---|---|
| 谁看得见哪些待办（"我的待办"、管理视图） | `backend/internal/service/service.go`（`ListMyTasks`、`ListTasksAdmin`） | `backend/internal/repo/tasks.go`（`ListTasks`、`ListInput`）、`backend/internal/repo/repo_test.go` |
| 谁能同意 / 驳回 | `backend/internal/service/service.go`（`actorInScope`） | `backend/internal/service/service_test.go` |
| 给待办加字段 | `migrations/`（新迁移） | `contracts/`（只增）、`buf generate`、`backend/internal/repo/tasks.go`（`taskSelectColumns`、`scanTask`）、两个 DTO |
| 新的状态变化 | `backend/internal/repo/actions.go`（`transitionTaskTx`） | `migrations/` 里的 CHECK 约束、`contracts/events/` 里的事件 |
| REST 响应形状或状态码不对 | `backend/internal/http/http.go` | `backend/internal/service/status.go`、`backend/internal/http/http_test.go` |
| 超期报告 | `backend/internal/repo/overdue.go` | `backend/module/module.go` 的 `startOverdueScan` |

## 构建与测试

```bash
# 测试连测试库 brickkit_test_db，绝不连 brickkit_db（项目根 make test-db-init 准备）
export TEST_PG_DSN="postgres://postgres:<口令>@localhost:5432/brickkit_test_db?sslmode=disable"
make test                    # 每个包都是 "ok"；没设 TEST_PG_DSN 直接失败
go test ./... -count=1 -v | grep -c -- '--- SKIP'   # 0：没有测试被跳过
make check-version dag-check contract-check import-scan module-check   # 每个打印一行 ✓
make docs-check              # "0 with errors, 0 warnings"
# 迁移，以登录角色运行：
PG_HOST=localhost PG_PORT=5432 PG_DATABASE=brickkit_test_db PG_USER=infra_workflow_rw \
  PG_PASSWORD=<口令> PG_SCHEMA=infra_workflow make migrate-idempotent   # ✓ 迁移幂等
```

在 BrickEnterprise 项目里口令是项目 `.env` 的 `POSTGRES_PASSWORD`；项目根 `make test-db-init ID=infra/workflow` 会准备好测试库并替你跑迁移幂等检查。`buf generate` 之后（契约变了），契约包要打新 tag：见易错点。真机在项目根跑 `make verify ID=infra/workflow ROUTE=/infra/workflow/tasks FOCUS=1`：构建镜像、只起本组件需要的东西、核对迁移 / 健康 / 权限判定、跑一次 focus、收尾。

## 设计取舍

- **待办箱，不是流程引擎。** 没有路由规则、没有业务数据、不回调：被指派人由调用方给，展示字段放在 `summary` 里传进来，结果以 `task.completed.v1` 发出去。
- **没有依赖、不消费事件。** 只有入边；`make dag-check` 遇到任何依赖就失败。
- **数据范围两维，都落在被指派人身上**：`assignee_sub`（owner）与 `assignee_dept_path`（org，前缀）。"我的待办"是 `owner OR org`；管理视图只判 `org`；gRPC 的 `ListTasks` 是系统视图，用 `AllDepts` 显式要"全部部门"。没分部门的调用者拿到的是 SDK 的哨兵前缀 `besdk.NoDeptPath`，匹配不到任何行，只剩 owner 一侧。
- **看得见不等于能处理**：部门子树里的主管看得见下属的待办；只有被指派人本人能同意或驳回。
- **命令 claim-first 幂等**，统一一张表 `command_idempotency`，以 `idempotency_key` 为键，并核对最先用它的是哪个命令。
- **超期只报告、不处理**：扫描设 `overdue_notified_at`、只发一次事件；状态仍是 `PENDING`。

## 易错点

| 不要 | 症状 | 原因 |
|---|---|---|
| 加一个替业务做判断的字段或分支（"金额超过 X 要多一级"） | 什么都不坏；下一个需求再加一个，待办箱变成谁也换不掉的隐形规则引擎 | 谁来审批是调用方的规则，它把被指派人传进来 |
| REST 列表上把 `ScopeOwner` 或 `ScopePrefix` 留空，或者用空前缀表示"全部部门" | `repo.ListTasks` 回 `ErrInvalidArgument`（`400`） | 空前缀匹配一切，在 `OR` 里一个"全匹配"操作数就让整个条件匹配一切；仓储层拒绝它（`TestListTasks_非系统视图ScopePrefix留空报InvalidArgument`）。两个操作数都从 `ScopeOf` 取；只有 gRPC 的系统视图设 `AllDepts` |
| 把空的 `dept_path` 当成"根节点、看全部" | 一个没分部门、只被授了查看权限的新账号看得到全公司的待办 | 真实路径总以 `/` 开头（根部门也是）；空串表示没分部门。be-sdk-go v0.5.0 把它变成哨兵 `besdk.NoDeptPath`；`TestListMyTasks_无部门的人只看到指派给自己的待办` 守着它 |
| 往 `assignee_dept_path` 里存不以 `/` 开头的值 | 所有没分部门的人共享同一个哨兵前缀，看得到所有存成它的待办 | `CreateTask` 拒绝它（`INVALID_ARGUMENT`）；调用方传被指派人的真实路径或空串，绝不传自己的 `ScopeFilter.Prefix` |
| 在 gRPC 路径上调 `besdk.ScopeOf` | 调用 panic（`500`） | gRPC 调用不带用户 claims；数据范围只在 REST 上 |
| 因为 `Task.InScope` 为真就让部门主管审批 | 主管替下属签了审批，没有任何"转办"记录 | `InScope` 是可见性；处理要求 `assignee_sub` = 调用者（`actorInScope`） |
| 对已经不是 `PENDING` 的待办的动作返回成功 | 调用方以为自己处理了；并发的另一个动作悄悄赢了 | 重放在更早的幂等那层就短路了；走到 `transitionTaskTx` 时状态不对是真冲突（`ErrNotPending`，REST `409`） |
| REST 上只用 `service.ToStatus` 翻译 `ErrNotPending` | 前端拿到 `400`，提示"请求有误"而不是"已被处理" | SDK 把 `FailedPrecondition` 映射成 `400`；`restStatus` 把它换成 `Aborted`（`409`） |
| 在超期扫描里改 `status`（自动通过、自动关闭） | 待办从所有人的待处理列表里消失，又不发 `task.completed.v1`：业务单据一直等下去 | 超期只是报告；升级、催办、自动通过是业务组件的规则 |
| 改了 `contracts/infra/workflow/v1/workflow.proto` 的注释或选项却不给契约包打 tag | 本地构建通过（`replace` 掩盖了它）；外壳拉 `infra-workflow/v2` 时对着旧的 `gen/` 编译 | `gen/infra/workflow/` 下的任何变化都要新 tag `gen/infra/workflow/v1.x.y`，根 `go.mod` 也要 require 它 |

## 改代码前自查

1. 这是业务规则吗（谁来审、审完怎么办）？它属于业务组件，不属于这里。
2. 我在加依赖、加消费的事件、加对别的组件的调用吗？停下：见设计取舍。
3. REST 的列表或详情：两个范围操作数都从 `ScopeOf` 取了吗？加一条真实建库、"别人的待办看不到"的测试，再加一条"没分部门的调用者"的。
4. 改契约？只增：新字段、新 rpc、新查询参数、新事件。绝不删或改类型。
5. `gen/` 变了吗？契约包要打新 tag，`go.mod` 要 require 它。
6. 新 REST 路由用 `besdk.GET` / `POST` 加 `assembly.yaml` 里的权限键注册。
7. 新规则 → 先写会失败的测试（涉及 SQL 的用真实数据库），再写代码。
8. 发布之后的第一次改动之前先升 `metadata.version`；BRICKKIT.md 与 `docs/design.md` 跟代码在同一个提交里更新。
