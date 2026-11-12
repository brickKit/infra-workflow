# infra/workflow

## 组件定位

统一待办：把各流程管理方（业务组件里 SDK 的流程内核，线协议第 17 章）按共享任务契约发布的任务投影汇总起来。
- **我的待办**：候选处理人包含我、待处理的任务，最早截止的在前；可按任务类型、法人筛选，游标分页。
- **已办**：我作出决定的任务，与我曾是候选、已结束的任务，最近的在前。
- **读取一个任务**：现在或曾经是它的候选、或是决定者才能看（否则 404），答复带动作入口（所属组件与它的标准路由 `POST /be/flow/tasks/{id}/decisions`，由组件推出，不取投影里的地址）与“可能不是最新”的标注。
- **发布者核对**：只收部署为每种任务类型选定的发布方的投影（以前的发布方只为它已有的任务）；别的、或不合共享任务契约的，拒绝并写安全审计，管理员能列出来。

本组件不保存决定、不转发动作：前端把同意、拒绝直接发给任务所属的组件，待办随那个组件的下一份投影改变。审批实例、分配、决定的检查在流程管理方；`paused` 的任务（没有候选）在所属组件的异常入口。

**3.0.0**（第一个重建的版本，契约 `contract/v3.0.0-alpha.1`）：我的待办、已办、读取任务、被拒的投影、发布者核对、按版本覆盖、可能不是最新、被拒投影的清理、整套恢复的核对（SDK 自带的）。

## 部署前准备

- 数据库：PostgreSQL 16 及以上；schema `infra_workflow`，属主角色 `infra_workflow_owner`（只供迁移）与运行角色 `infra_workflow_rw`（只有读写数据的权限），名字来自装配项目的注册表。三者由部署方在第一次启动前建好；表由本组件的迁移建，迁移里不建角色、不授权。
- 消息系统（`BUS_URL`）与实现实例（`IMPLEMENTATION_INSTANCE`）：本组件不发布事件；按 `WORKFLOW_TASK_SOURCES` 订阅每个发布方的流（组件 `erp/purchase` 的流 `erp.purchase` 上的 `erp.purchase_task.changed.v1` 与 `erp.purchase_task.heartbeat.v1`）。
- **`WORKFLOW_TASK_SOURCES`**：逗号分隔的 `<任务类型>=<发布方组件>[|<以前的发布方>…]`，例 `erp.purchase.order.approval=erp/purchase,erp.purchase.requisition.approval=erp/purchase,erp.payment.request.approval=erp/payment,erp.payment.entry.approval=erp/payment`。格式不对、一种任务类型写两次，成员不启动；不配时待办是空的。任务类型由发布方组件在它的 `BRICKKIT.md` 里列出。
- 授权：`infra.workflow.task.read` 授予所有要用待办的人（看到哪些任务由“候选包含我”决定，本组件不判断别的组件的授权）；`infra.workflow.admin` 给排障的管理员。
- 部署的服务证书（`SERVICE_TLS_*`）；身份与授权（`IAM_*`、`AUTHZ_GRPC_URL`、`DEPLOYMENT_ID`）；审计（`AUDIT_GRPC_URL`）。
- 低到中负载：适合外壳（`be/go-infra`）。

## 依赖说明

- 必需：数据库、消息系统、身份成员、授权成员。
- 只经事件：配置里的发布方（不声明依赖；没装的发布方的流是空的，订阅只是等着）。
- 可选：审计组件。
- 发布方不可达或事件积压：它的待处理任务照常显示，`WORKFLOW_STALE_AFTER`（缺省 15 分钟）里没有心跳也没有投影时标“可能不是最新”；本组件不在本地关闭任务。

## 配置指南

全部键见 `component.yaml` 的 `configSchema`（由 SDK 生成，测试核对）。

| 键 | 说明 |
| --- | --- |
| `WORKFLOW_TASK_SOURCES` | 每种任务类型选定的发布方与以前的发布方（见部署前准备） |
| `WORKFLOW_STALE_AFTER` | 多久没有心跳也没有投影，发布方的任务标“可能不是最新”（缺省 `15m`，是心跳间隔的三倍） |
| `DEPLOYMENT_ID`、`IAM_ISSUER`、`IAM_JWKS_URL`、`AUTHZ_GRPC_URL`、`SERVICE_TLS_CERT_FILE`、`SERVICE_TLS_KEY_FILE`、`SERVICE_TLS_CA_FILE`（必填） | 受保护的用户面与 mTLS 的系统面要用；部署身份、身份与授权的地址在外壳里是进程级的键 |
| `PG_HOST`、`PG_DATABASE`、`PG_USER`、`PG_PASSWORD`、`PG_SCHEMA`（必填） | 自己的数据库，密码是密钥；迁移另用 `PG_OWNER_USER`、`PG_OWNER_PASSWORD` |
| `BUS_URL`（必填，密钥）、`IMPLEMENTATION_INSTANCE`（必填） | 消息系统；实现实例是安装时生成的随机身份 |
| `AUDIT_GRPC_URL` | 审计组件的系统面；缺时审计记录留在本地，按协议 03 处理 |

## 契约索引

- **系统面**：没有本组件自己的服务（整套恢复的核对是 SDK 的标准服务）。
- **用户面**：`contract/openapi.yaml`。`GET /api/workflow/tasks`（我的待办）、`GET /api/workflow/tasks/done`（已办）、`GET /api/workflow/tasks/{id}?component=`（一个任务：任务是组件加身份）要 `infra.workflow.task.read`；`GET /api/workflow/rejected-projections` 要 `infra.workflow.admin`。资源没有维度。
- **事件**：不发布。订阅的载荷是线协议的共享任务契约 `schemas/flow/task.v1` 与心跳 `task_heartbeat.v1`。`component.yaml` 的 `events.subscribes` 列出已知发布方的前缀（`erp.purchase_task.*`、`erp.payment_task.*`）；实际订阅由 `WORKFLOW_TASK_SOURCES` 决定。

## 外壳声明

不是外壳。可以被 Go 的基础外壳（`be/go-infra`）承载。
