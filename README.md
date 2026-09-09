# infra-workflow · 审批待办中心

轻量待办箱——业务组件登记一条"要人做决定"（approval）或"要人去改数据"（exception）的任务，本组件维护待办列表与审批历史，完成后发事件回话。**三条铁律**（设计书 §6.6）：严禁包含业务规则判断（"这张单该谁审"是调用方的事）、严禁直接查询业务数据库（只存调用方传入的展示快照）、严禁反向同步调用业务组件（零出边，只发事件）。

## 它能做什么

- `infra.workflow.v1.WorkflowService`（gRPC，组件间协议）：`CreateTask`/`CloseTask`/`CancelTask`（写命令，claim-first 幂等）、`GetTaskStatus`（⭐ 支持按 `task_id` 或 `idempotency_key` 二选一查）、`BatchGetTasks`（防 N+1）、`ListTasks`（组件间协议，不做数据权限过滤）
- REST（人类操作，`/infra/workflow/**`）：
  - `GET /tasks`：我的待办——分配给自己、或分配给自己管辖部门内任何人的待办都看得到（OR，不是 AND）
  - `GET /tasks/{id}`：详情 + 审批历史
  - `POST /tasks/{id}/approve` / `POST /tasks/{id}/reject`：只有当事人本人能操作，不接受部门主管代批（"转办"是设计计划明确列出的未做功能）
  - `GET /admin/tasks`：全局视图，绕过 owner 维但不绕过 org 维，可选按 `assignee_sub` 精确过滤
- 后台循环：每分钟扫一次 `due_at` 已过仍是 `PENDING` 的待办，发 `infra.workflow.task.overdue.v1`（只报告"超期了"，绝不修改 `status`，见§6.6 铁律一）

⚠️ **`CreateTask`/`CloseTask`/`CancelTask` 永远不进 REST**——它们是组件间协议，不是人类操作。人能创建的待办只有一种可能："人代表某个业务组件创建"，那等于给了一条绕过业务规则的路。

## 需要哪些基础资源

| 资源 | 形态 | 为什么需要 | 怎么起 |
|---|---|---|---|
| PostgreSQL 16 | **A**（`kind: database`） | `workflow_tasks`/`workflow_task_actions`/`command_idempotency` 独占 schema `infra_workflow` | 装配仓库根目录 `make up` |
| NATS 2.10 | **A**（`kind: mq`） | 发布 `infra.workflow.task.*.v1` 四条事件——**本组件零消费**，一条事件都不订阅（§6.6 铁律三：监听业务事件本身就是嵌入业务规则） | 同上 |

⚠️ **本组件零强依赖零弱依赖**——在同步图上只有入边（`erp-sales` 是目前唯一的调用方，弱依赖，`CreateTask` 3 次补偿失败后建一条 exception 待办）。谁给本组件加一条 `dependencies.components` 都违反了它存在的设计前提。

## 怎么起来

```bash
# 装配仓库根目录
make up                        # 起 PostgreSQL/NATS 等默认基础资源
cd components/infra/workflow
go build -o build/migrate ./backend/cmd/migrate
PG_SCHEMA=infra_workflow DATABASE_HOST=localhost DATABASE_PORT=5432 \
  DATABASE_USER=postgres DATABASE_PASSWORD=<.env 里的 POSTGRES_PASSWORD> DATABASE_NAME=brickkit_db \
  ./build/migrate up
go run ./backend/cmd/server     # 单独跑：besdk.RunStandalone 读 component.yaml 的端口
```

或者用平台：`brickkit up`（装配仓库根目录，`components/infra/workflow` 登记为 submodule 且在 `brickkit.yaml` 里之后）。

## 怎么用

```bash
# 业务组件登记一条待办（gRPC，人不会直接调这个）
grpcurl -plaintext -d '{
  "idempotency_key": "order-123-exception-1",
  "type": "TASK_TYPE_EXCEPTION",
  "assignee_sub": "u_zhangsan", "assignee_dept_path": "/1/12/",
  "title": "订单 SO-123 补偿失败，需要人工处理",
  "summary_json": "{\"order_id\":\"SO-123\",\"amount\":\"999.00\"}",
  "source_component": "erp/sales", "source_aggregate": "sales_order", "source_id": "SO-123",
  "deep_link": "/erp/sales/orders/SO-123"
}' localhost:9201 infra.workflow.v1.WorkflowService/CreateTask

# 人在前端点开"我的待办"
curl -H 'Authorization: Bearer <应用 token>' http://localhost:8201/infra/workflow/tasks

# 点"同意"
curl -X POST -H 'Authorization: Bearer <应用 token>' -H 'Content-Type: application/json' \
  -d '{"comment":"同意"}' http://localhost:8201/infra/workflow/tasks/1/approve

# 业务组件确认异常已处理，主动关闭（gRPC，同样不给人类走）
grpcurl -plaintext -d '{"idempotency_key":"close-1","task_id":"1","comment":"已重新提交成功"}' \
  localhost:9201 infra.workflow.v1.WorkflowService/CloseTask
```

## 配置项

| 配置键 | 默认值 | 说明 |
|---|---|---|
| `pgSchema` | `infra_workflow` | 本组件的 PG schema |
| `otelBaseUrl` | `""` | 空 = Blackhole Exporter，零成本 |
| `iamJwksUrl` | `""` | JWT 本地验签的公钥来源，指向 `infra-iam-casdoor` |
| `authzBundleUrl` | `""` | 权限判定的 bundle 轮询地址，指向 `infra-authz` |

### 待办的两个维度：谁能看见，跟着"被指派人"走，不跟着"单据"走

`assignee_dept_path` 存的是**被指派人的部门**，不是来源单据所属部门（`assembly.yaml` 的 `data_scopes` 与 `workflow_tasks` 建表注释都反复强调这一条）——写反的症状是部门主管看不到自己下属手上那张跨部门单据的待办。`GET /tasks` 用 OR（owner 维或 org 维任一命中就看得到），`GET /admin/tasks` 绕过 owner 维只判 org 维。

### 为什么这么"薄"：三条铁律不是软约束

|  | 归本组件 | 归业务组件 |
|---|---|---|
| "这张单该谁审" | ❌ | ✅ 金额分级、逐级上报、矩阵审批全是业务规则 |
| "这条数据对不对" | ❌ | ✅ 本组件只存展示快照 JSONB，从不 join 业务库 |
| "审批完了该怎么办" | ❌ | ✅ 本组件只发 `task.completed.v1`，业务组件据此决定订单状态怎么变 |

**这是本组件回话给业务组件的唯一通道**：`task.completed.v1`（同意/驳回/`exception` 被关闭）。漏发一条的后果是业务单据永远停在"审批中"、没有任何报错。

## 参考实现

| 项目 | 看的模块 | 借鉴了什么 | 许可证 | 用法 |
|---|---|---|---|---|
| Odoo `mail.activity` | `date_deadline` 字段设计 | 截止时间是待办的一等字段，不是可选装饰——本组件的 `due_at` 正是补这一条之后才完整 | LGPL-3.0 | 借鉴设计判断，不引用代码 |
| erp-inventory（本仓库） | claim-first 幂等（`command_idempotency` 表）、`GetReservationStatus` 双查判据 | 统一的写命令幂等模式；`GetTaskStatus` 支持按 `idempotency_key` 反查（超时是唯一拿不到 `task_id` 的场景） | 内部代码，同项目复用 | 完整复用既有模式 |
| erp-sales（本仓库） | `Order.InScope`、`ListOrders` 的 `ViewMine` 判据 | `Task.InScope` 的 OR 判据（单条可见性校验）；`ListInput` 的两维过滤模型 | 内部代码，同项目复用 | 完整复用既有模式 |

## 边界与禁令

- **不做业务规则判断**——`assignee`/`assignee_dept_path` 由调用方算好传入，本组件只存不判
- **不查业务数据库**——`summary_json` 是调用方在 `CreateTask` 时给的展示快照，原样透传，本组件不理解它的内容
- **零出边**——不反向调用任何业务组件；`ListTasks`/`GetTaskStatus`/`BatchGetTasks` 是被调用方，不是发起方
- **零事件消费**——一条业务事件都不订阅，监听即嵌入业务规则
- **`workflow_tasks` 不分区**——待办量级远低于交易流水，但这个假设有前提（客户把审批开到"每单必审"时量级会逼近订单量），见 `docs/design/infra-workflow.md` §9 第 2 条
- **`CreateTask`/`CloseTask`/`CancelTask` 永远不进 REST**——人能创建的待办只有"代表业务组件创建"这一种可能，等于绕过业务规则
- **approve/reject 只认当事人本人**——不接受部门主管代批，"转办"是未做功能
