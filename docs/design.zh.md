[English](design.md) · [中文](design.zh.md)

# infra/workflow 设计

只写结论，给要改本组件设计的人看。怎么用：`BRICKKIT.zh.md`；代码怎么组织：`AGENTS.zh.md`。

## 边界

本组件是待办箱：存需要人处理的待办，让那个人处理它，再把结果报告出去。三条规则决定了它的一切：不包含任何业务规则、从不读业务数据库、从不反向调用业务组件。

| 不在这里 | 归谁 | 为什么 |
|---|---|---|
| 一张单据该谁审批（金额分级、逐级上报、审批矩阵） | 创建待办的业务组件 | 这是业务规则。调用方算好被指派人传进来；这是最容易被侵蚀的边界，因为每多一条规则看起来都很小 |
| 审批之后怎么办 | 业务组件，监听 `task.completed.v1` | 回调它就是一条从基础设施指向业务的同步边，也是待办箱开始懂业务语义的地方 |
| 待办里展示的数据（客户名、金额……） | 调用方，以快照放进 `summary` | join 业务表会让待办箱跟每个业务 schema 绑死 |
| 通知人 | 通知组件，监听 `task.created.v1` / `task.overdue.v1` | 登记待办不能因为某个通知渠道挂了或没装而失败 |
| 失败业务操作的补偿与重试 | 发起这个操作的组件 | 在这里放编排引擎就得反向调用业务组件 |

两类待办。`APPROVAL` 要一个决定。`EXCEPTION` 要人去改数据：刻意不给"重试"按钮——重试意味着再次触发业务操作（反向调用），而且因为数据不对而失败的操作原样重试只会再失败。闭环是：人顺着 `deep_link` 到业务界面，在那边改好数据、重新提交；业务组件成功之后调 `CloseTask`。

看起来很小、却会改变本组件性质的需求："金额超过 X 要多一级"（业务组件算出审批链，每一级建一条待办）、"休假时自动转给副手"（组织数据，不在这里）、"列表里显示客户名"（放进 `summary` 传进来）。

## 拥有的数据

| 表 | 分区 | 说明 |
|---|---|---|
| `workflow_tasks` | 否 | 类型、状态、`assignee_sub`、`assignee_dept_path`（被指派人的部门，不是单据的部门）、标题、`summary` JSONB、来源组件 / 聚合 / ID、`deep_link`、`due_at`（可空）、`overdue_notified_at`、`version` |
| `workflow_task_actions` | 否 | 处理历史：处理人、动作（`APPROVED`、`REJECTED`、`CLOSED`、`CANCELLED`）、附言、时间；只增不改 |
| `command_idempotency` | 否 | `idempotency_key` 主键、用它的命令、产生的待办 ID |
| `event_outbox` | 按 `created_at` 周分区 | 推送循环从这里发布；分区由组件自己提前四周建好。没有 `event_inbox`：不消费任何事件 |

状态：`PENDING` 是唯一的活跃态；`APPROVED`、`REJECTED`、`RESOLVED`（被 `CloseTask` 关闭）、`CANCELLED` 是终态。每次状态变化都在同一个事务里锁行、确认仍是 `PENDING`、改状态、追加一条历史、发事件；所以一条待办最多只有一条历史。

`due_at` 是一等字段：没有它，"超期未审批"这个待办箱最常见的诉求就无从表达。超期了怎么办不由本组件决定，它只报告。

## 契约面

gRPC `infra.workflow.v1.WorkflowService`，给业务组件用：

| rpc | 类型 | 说明 |
|---|---|---|
| `CreateTask` | 命令 | 按 `idempotency_key` 幂等 |
| `CloseTask` | 命令 | 幂等；要带 `task_id`（调用方从 `CreateTask` 起就一直持有）；→ `RESOLVED` |
| `CancelTask` | 命令 | 幂等；要带 `task_id`；→ `CANCELLED` |
| `GetTaskStatus` | 读 | 按 `task_id` **或**按 `CreateTask` 的 `idempotency_key`；总是回带 `task_id` |
| `BatchGetTasks` | 读 | 查不到的 ID 省略 |
| `ListTasks` | 读 | 按类型 / 状态 / 来源组件筛；按待办 ID 游标分页 |

幂等是 claim-first：写之前在同一个事务里对 `command_idempotency` 做 `INSERT … ON CONFLICT DO NOTHING`，同一个 key 的两个并发请求不可能各建一条待办（"先查再插"有这个窗口）。同一个命令重放返回第一次的结果。被别的命令用过的 key 报 `INVALID_ARGUMENT`：照样短路的话，`CloseTask` 会拿到 `CreateTask` 的结果，调用方以为一条仍在等待的待办已经关了。

`GetTaskStatus` 能按 `idempotency_key` 查，是因为超时恰恰是调用方没收到 `task_id` 的情形。查不到（`TASK_STATUS_UNSPECIFIED`）与 `CANCELLED` 永不合并：前者说明请求根本没到、可以重试，后者说明重试是错的。

对已经不是 `PENDING` 的待办，任何动作都失败（gRPC `FAILED_PRECONDITION`）：重放走不到状态检查，走到了却状态不对，说明 `task_id` 拿错了，或者并发的另一个动作抢先了。gRPC 的读不按数据范围过滤：组件之间的调用不带用户身份，在那里调 `besdk.ScopeOf` 会 panic。

REST，前缀 `/infra/workflow`，给人用：

| 路径 | 权限键 | 说明 |
|---|---|---|
| `GET /tasks` | `infra.workflow.task.view` | "我的待办"：owner OR org（见数据范围） |
| `GET /tasks/{id}` | `infra.workflow.task.view` | `TaskDetail`：待办加 `actions`（还没人处理时是空数组）；不在调用者范围内 `403`，不存在 `404` |
| `POST /tasks/{id}/approve` | `infra.workflow.task.act` | 只认被指派人本人；回改完之后重新读出的 `TaskDetail` |
| `POST /tasks/{id}/reject` | `infra.workflow.task.act` | 同 approve；必须带 `comment`（没有就 `400`） |
| `GET /admin/tasks` | `infra.workflow.admin` | 只判 org，可选 `assignee_sub` |

`CreateTask`、`CloseTask`、`CancelTask` 永远不进 REST：人建待办只可能是"人代表某个业务组件建"，那就绕过了它的规则。对已经不是 `PENDING` 的待办 approve / reject 回 `409`：SDK 把 `FAILED_PRECONDITION` 映射成 `400`，所以 REST 层把这一个错误翻成 `ABORTED`（`409`），界面据此提示"已被处理，刷新一下"而不是"请求有误"。approve / reject 回完整的 `TaskDetail`，改完之后重新读一遍，界面直接拿响应刷新：新状态、新的 `updated_at`、刚记下的那条历史都在里面。

看得见不等于能处理。`GET /tasks/{id}` 与列表用同一个 OR（主管看得见下属的待办）；approve / reject 要求 `assignee_sub` = 调用者。转办、加签都还没做，所以主管没有任何途径替别人处理。

## 发布与消费的事件

发布，全部经 outbox，与状态变化在同一个事务里：

| Subject | 分级 | 何时 | payload |
|---|---|---|---|
| `infra.workflow.task.created.v1` | 核心 | `CreateTask` | 待办 ID、被指派人、标题、类型、来源 |
| `infra.workflow.task.completed.v1` | 核心 | 同意、驳回、`CloseTask` | 待办 ID、动作（`APPROVED` / `REJECTED` / `RESOLVED`）、处理人（`CloseTask` 时为空）、附言、来源 |
| `infra.workflow.task.cancelled.v1` | 核心 | `CancelTask` | 待办 ID、原因、来源 |
| `infra.workflow.task.overdue.v1` | 旁路 | `PENDING` 的待办过了 `due_at`，只发一次 | 待办 ID、被指派人、`due_at` |

`completed` 是处理结果回到业务组件的唯一通道；丢一条，单据就永远停在"审批中"而没有任何报错，所以三条状态事件都是核心、都走 outbox。`cancelled` 是独立的 subject，不是 `completed` 的一种：靠 `completed` 推进单据的业务组件不能把作废当成审批决定。

消费：无。消费业务事件就要建立"这个事件关掉那条待办"的映射，那是业务规则；单据作废由它的组件调 `CancelTask`。

## 依赖

无，设计如此。预期中却不存在的依赖：

| 不是依赖 | 为什么 |
|---|---|
| 任何业务组件 | 只有它们调本组件；反向的边会让成环成为可能，也会把业务含义放进这里 |
| 通知组件 | 用事件耦合；它可能没装，登记待办不能等它 |
| 组织主数据组件 | `assignee_dept_path` 是调用方给的快照，按前缀比较；不读任何组织树 |
| `infra/authz`、`infra/iam-casdoor` | 权限 bundle 与 JWKS 来自 `AUTHZ_BUNDLE_URL` / `IAM_JWKS_URL`：是配置，不是边 |

## 在同步调用图里的位置

只有入边：订单补偿一直失败时 `erp/sales` 调 `CreateTask`（可选依赖），移动端 BFF 代用户读 `/infra/workflow/tasks`。它谁也不调，所以不可能处在环里。作为 CRM 与 ERP 都可能调用的基础设施，并不破坏"CRM 与 ERP 之间没有同步边"：本组件两边都不是。

## 分区与归档

`workflow_tasks` 不分区：只有需要人介入时才产生待办，远少于交易流水。这个假设在客户把每张单据都设成要审批时不成立，那时量级会逼近订单量；而加分区键是数据迁移，不是小改动。`event_outbox` 按周分区；`partition.Start` 维持当前周加未来四周，缺分区会让每次 outbox 写入失败。

预期的保留策略：待办在终态**且**超过十二个月时归档到 `infra_workflow_archive`（绝不只看时间：一条很久以前建、至今没人审的 `PENDING` 待办正是最需要被看见的），历史跟着它的待办走；`command_idempotency` 九十天后删除；已发布的 outbox 行三十天后清理。这些都还没有实现；归档 schema 里没有任何写入。

## 数据范围

`workflow_tasks` 上两维，都关于**被指派人**：`owner` = `assignee_sub`（等值）与 `org` = `assignee_dept_path`（前缀）。可见性跟着待办指派给的那个人走，不跟着单据的部门走；写反了，主管就看不到下属手上那张跨部门单据的待办。

- "我的待办"（`GET /tasks`）：`assignee_sub = 我 OR assignee_dept_path LIKE 我的 dept_path || '%'`。OR 写在契约里。两个操作数都来自调用者的 `ScopeOf`；把前缀留空，它就变成全匹配，OR 就变成"人人看到全部"。
- 管理视图（`GET /admin/tasks`）：只判 `org`，绕过 `owner`、不绕过 `org`：调用者的部门子树，不是整个租户。
- `dept_path` 为空的调用者坐在组织树根节点，看得到全部；这是 SDK 对"不限"的定义，不是缺值。
- 两条都有真实建库的测试：别的部门里别人的待办绝不出现在"我的待办"里（`TestListTasks_我的待办用OR不是AND`、`TestListMyTasks_service层注入两维ScopeOf`），详情在范围外回 `403`。

## 参考实现

| 项目 | 版本 | 看的模块 | 借鉴了什么 | 许可证 | 用法 |
|---|---|---|---|---|---|
| Odoo | — | `mail.activity` | 轻量待办箱的形状：多态挂到任意记录、负责人、`date_deadline`、摘要、活动类型；截止时间是一等字段 | LGPL-3 | 借鉴思路 |
| ERPNext | — | `ToDo` 与 `Workflow` doctype | 反例：`Workflow` 把待办和流转规则放在同一层，条件存成 Python 表达式 | GPL-3 | 借鉴思路 |
| Camunda / Flowable | — | BPMN 引擎的定位 | 没有采用：它的价值在流程建模与长事务状态，而这里的补偿在发起它的业务组件手里 | Apache-2.0 | 借鉴实践 |
| 钉钉审批 / 飞书审批 | — | 审批单形态、审批人配置 | 实际用的是"谁审、能不能加签、能不能撤回"，很少用复杂分支：轻量形态够用 | 闭源 | 借鉴实践 |

**刻意避开的**：把条件存成运行时表达式（测不了，AI 也读不懂）、靠查业务库渲染待办列表、审批完回调业务组件。Temporal 一类的工作流引擎也没有采用：它们跨服务持久化并重放长事务，会把待办箱变成必须调用业务组件的编排中枢。

不是槽位族：边界没有给"几种都合理的待办箱形态"留空间。真正会变的审批路由，在算被指派人的业务组件里。

## 未决问题

| 问题 | 当前答案 |
|---|---|
| 多级审批：调用方一次传整条链，还是每一级建一条待办？ | 每一级一条，由业务组件在上一级完成后建；传整条链会把"下一个是谁"搬进本组件 |
| `workflow_tasks` 要不要分区？ | 现在不分；按真实数据量复核，而且要早判，因为加分区键是数据迁移 |
| 转办、加签、撤回 | 没做。转办只改 `assignee_sub`（本组件的数据）；谁能转给谁是组织规则（不在这里） |
| 归档与清理（见分区与归档） | 没有实现；目前数据量很小 |
| "我的待办"按待办 ID 排序，虽然有一个 `(status, due_at)` 索引是给"超期优先"排序准备的 | ID 排序让游标简单；按 `due_at` 排要把它放进键集。与前端一起定 |
