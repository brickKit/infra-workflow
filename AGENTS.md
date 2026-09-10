# infra-workflow · AI 助手导读

## 身份证

| 项 | 值 |
|---|---|
| 组件 ID | `infra/workflow` |
| 仓库名 | `infra-workflow` |
| 端口 | HTTP `8201` / gRPC `9201`（`registry/ports.tsv`，装配仓库根目录那份） |
| schema / role | `infra_workflow` / `infra_workflow_rw`（归档 schema `infra_workflow_archive`，本组件目前不归档任何数据——待办量级远低于交易流水） |
| 语言 / 框架 | Go：Gin + `database/sql` + `pgx/v5/stdlib` + `golang-migrate` |
| 合并部署时进 | 外壳三 `go-infra` |
| 装配角色 | `default` |
| 设计真相源 | 装配仓库 `docs/design/infra-workflow.md`——本文件与它冲突时，以那份为准，回来改这里 |

## 边界

**归我：** 待办箱本身——登记（`CreateTask`）、审批历史、状态流转（同意/驳回/关闭/作废）、完成后回话事件（`task.completed.v1`）、超期报告（`task.overdue.v1`）。设计书 §6.6 的三条铁律是本组件存在的设计前提：

| 什么 | 归谁 | 为什么 |
|---|---|---|
| "这张单该谁审"（金额分级、逐级上报、矩阵审批） | 业务组件 | 铁律一。这条是本组件最容易被侵蚀的边界——**任何"帮业务组件多算一点"的提议都要先停下来问一句这是不是审批路由** |
| "这条数据对不对"（业务规则校验） | 业务组件 | 铁律二。本组件只存调用方传入的展示快照 JSONB，从不 join 任何业务库 |
| "审批完了该怎么办"（订单状态怎么变） | 业务组件 | 铁律三。本组件只发 `task.completed.v1`，业务组件监听后自己决定；本组件绝不反向调用业务组件的接口 |
| 待办的通知推送（钉钉/邮件/站内信） | `infra-notification`（阶段三下一个任务） | 本组件只发事件，谁去通知、通知渠道优先级都不归本组件 |

⚠️ **`data_scopes` 两维**：`owner`（`assignee_sub`，等值）+ `org`（`assignee_dept_path`，前缀）——两维都落在**被指派人**身上，不是来源单据。写反的症状是部门主管看不到下属手上跨部门单据的待办。

## 契约面与事件

**gRPC `infra.workflow.v1.WorkflowService`：** `CreateTask`/`CloseTask`/`CancelTask`（命令，claim-first）、`GetTaskStatus`（⭐ 按 `task_id` 或 `idempotency_key` 二选一）、`BatchGetTasks`（防 N+1）、`ListTasks`（组件间协议，不做数据权限过滤——本项目目前没有任何组件在 gRPC 侧转发/验证 JWT）。

**REST：** `/infra/workflow/**` 前缀。`GET /tasks`（我的待办，OR 语义）、`GET /tasks/{id}`（详情+历史）、`POST /tasks/{id}/approve|reject`（只认当事人本人）、`GET /admin/tasks`（全局视图，绕过 owner 维不绕过 org 维）。**`CreateTask`/`CloseTask`/`CancelTask` 永远不进 REST**——人代表业务组件创建待办等于绕过业务规则。

**发布事件：** `infra.workflow.task.created.v1`/`.completed.v1`/`.cancelled.v1`（核心，⭐ `completed` 是本组件回话给业务组件的唯一通道）、`.overdue.v1`（旁路，只报告不处理）。
**消费事件：** 无——一条都不消费（铁律三：监听业务事件本身就是嵌入业务规则）。

## 依赖与「为什么不依赖某某」

`dependencies.components` 永远是空数组，本组件在同步图上只有入边。

- **不依赖任何业务组件**：反过来，`erp-sales` 是目前唯一的调用方（弱依赖，`optional: true`，`ConfirmOrder` 补偿失败 3 次后建一条 `exception` 待办）——这条边的方向永远是"业务组件 → 本组件"，绝不会反过来。
- **不依赖 `mdm-org`**：本组件不持有组织架构主数据，`assignee_dept_path` 是调用方在 `CreateTask` 时给的字符串快照，本组件原样存原样比对前缀，不查任何组织树。
- **不依赖 `infra-notification`**：事件是唯一的耦合方式，本组件不知道、也不该知道谁在监听 `task.created.v1`。

## 这个组件特有的坑

| 不许 | 症状 | 出处 |
|---|---|---|
| 给 `workflow_tasks` 加任何业务规则判断字段（比如"金额阈值"、"审批级别"） | 没有立刻的症状，但下一次改需求就会有人想在这里加 `if amount > threshold`——铁律一从此开始被侵蚀，最终变成一个隐藏的业务规则引擎 | §6.6 铁律一、README「为什么这么薄」 |
| `CreateTask`/`CloseTask`/`CancelTask` 三个命令用一个共享的 `idempotency_key` 列直接建在 `workflow_tasks` 上 | 编译、迁移都能过——但这是本次实现中犯过一次的真实设计错误：`idempotency_key` 是命令的输入回执，不是待办这个资源的持久属性，正确做法是统一走 `command_idempotency` 一张表（同 `erp-inventory` 的既有先例），已在写代码阶段自己发现并改正，proto 里字段号 13 因此被刻意留空不用 | `migrations/001_create_workflow.up.sql`、`contracts/infra/workflow/v1/workflow.proto` 的注释 |
| `CloseTask`/`CancelTask` 的 `task_id` 做成"二选一"（模仿 `GetTaskStatus`） | 看起来更"统一"，但语义是错的——调用方从 `CreateTask` 的返回值起就一直持有 `task_id`，`idempotency_key` 只保证这一次关闭/作废命令本身重放安全，不用来反查资源。`GetTaskStatus` 的二选一解决的是完全不同的问题（超时时连 `task_id` 都还没拿到） | `contracts/infra/workflow/v1/workflow.proto` 的 `CloseTaskRequest` 注释 |
| 让 `Task` 消息同时被 `CreateTask`/`CloseTask`/`CancelTask` 三个 rpc 直接当返回值 | `buf lint` 会用 `RPC_REQUEST_RESPONSE_UNIQUE` 类规则拦下来——同一个消息类型不能被多个 rpc 复用；`CreateTask` 占了"读接口直接返回聚合根本身"的名额，其余需要返回同样形状的 rpc 必须各自包一层 `XxxResponse` | `contracts/infra/workflow/v1/workflow.proto` |
| `ListTasks`（gRPC）里调 `besdk.ScopeOf(ctx)` 想着"顺手把数据权限也做了" | 会直接 panic——`ScopeOf` 要求 ctx 里有 `RequirePermission` 验签过的 Claims，而 gRPC 组件间调用（`SystemClient`）不会带；本项目目前没有任何组件在 gRPC 侧转发/验证 JWT。数据权限只在 REST 面（`service.ListMyTasks`/`ListTasksAdmin`）生效 | `backend/internal/service/service.go` 的 `ListTasks` 注释 |
| `service.ListMyTasks`/`ListTasksAdmin` 只置 `ScopeOwner` 或只置 `ScopePrefix` 中的一个，让另一个留空参与 OR | **这是本次实现中真实出现过的一个数据权限漏洞**：`ScopePrefix` 留空时 SQL 的 `LIKE '' || '%'` 会匹配全部部门，OR 之后等于"我的待办"看到所有人的待办。两个字段必须都从 `besdk.ScopeOf(ctx)` 真实派生——OR 的两个操作数缺一个都是 fail-open，不是"看少了" | `backend/internal/service/service.go`、`backend/internal/repo/repo_test.go` 的 `TestListTasks_我的待办用OR不是AND` |
| approve/reject 时判"调用者在待办的 org 范围内"就放行 | 那是 `GetTaskDetail` 的可见性判据（`Task.InScope` 的 OR），不是"能不能替他做决定"——approve/reject 必须严格要求 `assignee_sub == 调用者自己`，"转办"是设计计划明确列出的未做功能，本阶段不接受部门主管代批 | `backend/internal/service/service.go` 的 `actorInScope` |
| 对一个已经不是 `PENDING` 的待办再次 approve/reject/close/cancel 时"静默忽略、直接返回成功" | 看起来更"友好"，但 claim-first 已经在更上一层保证了"同一个命令重放"不会走到这里；真的走到这里却状态不对，说明调用方拿错了 `task_id` 或者有并发的另一个动作抢先了，两种情况都值得报错而不是悄悄吞掉。统一策略：一律返回 `ErrNotPending` | `backend/internal/repo/actions.go` 的 `transitionTaskTx` |
| 超期扫描（`MarkOverdueAndPublish`）顺手把 `status` 也改了（比如自动标记成某种"已超期"状态） | 违反铁律一——本组件只报告"超期了"，怎么处理（升级/催办/自动通过）归业务组件或 `infra-notification`。`PENDING` 待办超期之后仍然是 `PENDING`，只是多了一条 `overdue_notified_at` 标记和一条事件 | `backend/internal/repo/overdue.go` |
| 给 `workflow_tasks` 加分区 | 待办量级远低于交易流水，但这条判断有前提——客户把审批开到"每单必审"时量级会逼近订单量，见设计计划 §9 第 2 条，实现后要按真实数据复核，不是这里可以随便改的假设 | `docs/design/infra-workflow.md` §9 |
| 忘记给 `event_outbox` 接周分区维护循环 | **真机发现过的真实缺口**：`event_outbox` 从建仓库起就是分区表（决策 54，跟其余所有组件一样），但最初的 `Module.Start` 只顾着"`workflow_tasks` 不分区不需要维护"，漏了 `event_outbox` **确实**分区、也需要维护——迁移只种了 4 周初始分区，第 5 周起对应日期没有分区，Outbox 写入会报"找不到分区"直接失败，且没有任何清晰指向"分区没建"的报错，所有新的 `task.created/completed/cancelled` 事件都发不出去。已补 `backend/internal/partition`（同 `erp-sales` 复用的同一套判据）并接入 `Start`，真机对运行中的容器验证过确实建出了新分区（阶段三 Task 13 旁路发现并修复，v1.0.0→v1.0.1） | `backend/internal/partition/partition.go`、`backend/module/module.go` |

## 改代码前的自查

1. **我是不是在往 `workflow_tasks` 加一个"判断"字段？** 停下——那几乎总是业务规则该待的地方，不是这里。
2. **我是不是在给某个写命令的幂等单独建一列，而不是走 `command_idempotency`？** 停下——统一走那一张表，见上表的既有教训。
3. **我改的 proto，是不是让同一个消息被多个 rpc 当返回值？** `buf lint` 会拦，除了 `CreateTask` 那个名额，其余都要包一层 `XxxResponse`。
4. **我是不是在 gRPC 层调了 `besdk.ScopeOf`？** 停下——会 panic，数据权限只在 REST 层的 `service.ListMyTasks`/`ListTasksAdmin` 生效。
5. **我改 `ListMyTasks`/`ListTasksAdmin` 时，`ScopeOwner`/`ScopePrefix` 是不是两个都真的从 `ScopeOf(ctx)` 取了值？** 停下并核对——漏一个就是本组件出现过的那个真实数据权限漏洞。
6. **我是不是在给 approve/reject 放宽成"org 范围内就能操作"？** 停下——那是可见性判据，不是操作权限判据，必须是当事人本人。
7. **我是不是在给某个状态流转失败的路径改成"静默返回成功"？** 停下——统一策略是报 `ErrNotPending`，不悄悄吞掉。
8. **我是不是在给本组件加一条事件消费、或一条指向业务组件的调用？** 停下——铁律三，零出边零消费是硬约束。
