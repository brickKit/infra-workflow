# infra/workflow

## 组件定位

轻量待办箱。业务组件把"需要人来处理"的事登记进来：`APPROVAL`（要人做决定：同意或驳回）或 `EXCEPTION`（要人去业务界面改数据）。本组件维护每个人的待办箱与处理历史，处理结果通过事件回给业务组件。它不判断该谁审批、不读业务数据、不反向调用业务组件。

**拥有**

- 待办：类型、状态（`PENDING`，之后是 `APPROVED`、`REJECTED`、`RESOLVED`、`CANCELLED` 之一）、被指派人（`assignee_sub`）与被指派人的部门路径（`assignee_dept_path`）、标题、展示快照（`summary`，调用方给的 JSON，原样透传）、来源（组件、聚合、ID）、跳回业务界面的 `deep_link`、可选的截止时间 `due_at`。
- 每条待办的处理历史（谁、做了什么、附言、时间），只增不改。
- 事件 `infra.workflow.task.created.v1`、`infra.workflow.task.completed.v1`、`infra.workflow.task.cancelled.v1`、`infra.workflow.task.overdue.v1`。

**不拥有**

- 一张单据该谁审批（金额分级、逐级上报、审批矩阵）：由创建待办的业务组件算好被指派人再传进来。
- 审批之后怎么办（过账、发货、解冻）：业务组件监听 `infra.workflow.task.completed.v1` 自己决定。
- 待办里展示的业务数据：调用方在创建待办时把要展示的字段放进 `summary`；本组件从不 join、不读任何业务库。
- 通知人（钉钉、邮件、站内信）：通知组件监听 `infra.workflow.task.created.v1` 与 `infra.workflow.task.overdue.v1`。
- 组织架构：`assignee_dept_path` 是调用方给的字符串快照，只按前缀比较。

## 部署前准备

- **PostgreSQL**：schema `infra_workflow`，按你的 schema 约定可能还有 `infra_workflow_archive`（本组件从不写它）。登录角色 `infra_workflow_rw`，在 `infra_workflow` 上有 `USAGE` 与 `CREATE`，以及它的密码。迁移以这个角色运行并建表，所以表归它所有；运行中的组件自己建 `event_outbox` 的周分区，需要这份所有权。BrickKit 不建这些；在 BrickEnterprise 装配项目里，`make dev-env` 把密码写进 `.env`，`make db-init` 建 schema、角色与授权。
- **NATS** 在 `NATS_URL` 可达：事件先进 outbox 表，由后台循环推送。NATS 不可达时组件照样启动，事件在 outbox 里等。
- **权限**（`infra/authz`）与**身份**（`infra/iam-casdoor`，或任何提供 JWKS 的 IAM）在 `AUTHZ_BUNDLE_URL` 与 `IAM_JWKS_URL` 可达，REST 路由才会回错误以外的东西。它们是配置，不是依赖：没有它们组件照样启动。gRPC 服务不带用户身份，不需要它们。
- 没有自己的示例数据：待办由业务组件经 gRPC 登记。

## 依赖说明

无，而且按设计永远不会有：业务组件调本组件（gRPC `CreateTask`、`CloseTask`、`CancelTask`），本组件只用事件回话。它不消费任何事件：建立"哪个业务事件关掉哪条待办"的映射，就是把业务规则放进了这里。在 BrickEnterprise 项目里 `erp/sales` 以可选依赖用它：订单补偿一直失败时开一条 `EXCEPTION` 待办。

权限 bundle 与 JWKS 从配置里的地址拉取，不是依赖边。没有它们，每个受保护路由都失败关闭：没带或带了无效 token → `401`；`IAM_JWKS_URL` 为空或不可达 → `403`；bundle 还没拉到过 → `503`；合法用户但没有权限键 → `403`。`/healthz` 始终是 `200`：它只代表本进程活着。

## 配置指南

| 变量 | 含义 |
|---|---|
| `PG_HOST` | PostgreSQL 主机。通常用项目的共享值（`$var:PG_HOST`）。 |
| `PG_PORT` | PostgreSQL 端口；默认 `5432`。 |
| `PG_DATABASE` | 放 `infra_workflow` schema 的数据库（`$var:PG_DATABASE`）。 |
| `PG_USER` | 登录角色，写字面量 `infra_workflow_rw`。进外壳后外壳以自己的角色登录，每个事务切到这个角色（`SET LOCAL ROLE`），所以角色名必须是 `<PG_SCHEMA>_rw`。 |
| `PG_PASSWORD` | `PG_USER` 的密码。密钥：写 `${INFRA_WORKFLOW_DB_PASSWORD}`（或你的密钥库引用），绝不写值。 |
| `PG_SCHEMA` | 全部表、outbox 与迁移状态表（`schema_migrations_infra_workflow`）所在的 schema；默认 `infra_workflow`。仍然写上字面量，好让每个组件的 schema 在一处看得见。 |
| `NATS_URL` | outbox 推送循环发布事件的 NATS 地址（`$var:NATS_URL`）。 |
| `OTEL_BASE_URL` | OpenTelemetry collector 的基础地址；为空（默认）不导出。 |
| `AUTHZ_BUNDLE_URL` | 权限判定轮询的 bundle 地址，例如 `http://infra-authz-2-0-0:8223/authz/bundle`。必填：没有它每个受保护路由都回 `503`。要与项目里运行的 authz 版本保持一致。 |
| `IAM_JWKS_URL` | 本地验用户 token 用的 JWKS 地址，例如 `http://infra-iam-casdoor-2-0-0:8200/.well-known/jwks.json`。必填：没有它每个受保护路由都回 `403`。 |

## 契约索引

- `contracts/infra/workflow/v1/workflow.proto` — gRPC `infra.workflow.v1.WorkflowService`，给业务组件用：`CreateTask`、`CloseTask`（业务组件自己关闭待办，典型是 `EXCEPTION` 的数据改好之后 → `RESOLVED`）、`CancelTask`（来源单据作废 → `CANCELLED`）。`CreateTask` 的 `summary_json` 可选（不填存成 `{}`）；给了就必须是合法 JSON，否则报 `INVALID_ARGUMENT`。每个命令都带 `idempotency_key`；同一个命令重放返回第一次的结果，被别的命令用过的 key 报 `INVALID_ARGUMENT`。`CloseTask` / `CancelTask` 要带 `task_id`；待办已经不是 `PENDING` 时报 `FAILED_PRECONDITION`。读：`GetTaskStatus`（按 `task_id`，或按 `CreateTask` 的 `idempotency_key`——用于 `CreateTask` 超时的情形；`TASK_STATUS_UNSPECIFIED` 表示查不到，与 `CANCELLED` 不同）、`BatchGetTasks`（查不到的 ID 省略）、`ListTasks`（按类型、状态、来源组件筛；游标分页，`page_size` 1–200，默认 50）。gRPC 不带用户身份，这些读不按数据范围过滤。Go 包是独立模块 `github.com/brickKit/infra-workflow/gen/infra/workflow`。
- `contracts/workflow.openapi.yaml` — REST，前缀 `/infra/workflow`，给人用，每个路由都带权限键：`GET /tasks`（`infra.workflow.task.view`；"我的待办"：指派给我的，或指派给我部门子树里任何人的；按 `type`、`status` 筛；`cursor`、`page_size`），`GET /tasks/{id}`（`infra.workflow.task.view`；待办加处理历史 `actions`；不在调用者范围内回 `403`），`POST /tasks/{id}/approve` 与 `POST /tasks/{id}/reject`（`infra.workflow.task.act`；只认被指派人本人；驳回必须带 `comment`；回待办加处理历史；待办已经不是 `PENDING` 回 `409`），`GET /admin/tasks`（`infra.workflow.admin`；调用者部门子树里的全部待办，不论指派给谁，可选 `assignee_sub`）。建待办、关闭、作废不在 REST 上。
- `contracts/events/workflow.events.json` — 经 outbox 发布的事件：`infra.workflow.task.created.v1`（登记了一条待办）、`infra.workflow.task.completed.v1`（同意、驳回，或被 `CloseTask` 关闭；处理结果回到业务组件的唯一通道）、`infra.workflow.task.cancelled.v1`（`CancelTask`）、`infra.workflow.task.overdue.v1`（`PENDING` 的待办过了 `due_at`；每条只发一次，状态不变）。不消费任何事件。
- `assembly.yaml` — 本项目的元数据：上面三个权限键、菜单项"我的待办"、网关路由 `/infra/workflow/**`、schema 与角色，以及 `workflow_tasks` 上的数据范围 `owner` = `assignee_sub`（等值）与 `org` = `assignee_dept_path`（前缀）。

## 外壳声明

不是外壳。可以由 Go 外壳托管（在 BrickEnterprise 项目里是 `be/go-infra`），也可以独立运行；两种方式代码相同。
