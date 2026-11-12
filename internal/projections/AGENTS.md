# projections：任务投影与心跳

收部署选定的发布方的任务投影与心跳（`docs/design.md` 的 WF01、WF02、WF06、WF09）。

## 负责与不负责

- 负责：表 `task`、`task_candidate`、`publisher`、`rejected_projection`；订阅（`Subs`：配置里每个发布方的流，投影与心跳两种类型）；`OnEvent`（心跳记下时刻；投影按共享任务契约核对形状、组件对得上信封的来源、身份是任务身份，否则 `malformed`；发布者核对 `sources.Judge`；按版本整体覆盖，现在的候选替换、曾经的只追加；发布方的最后一份投影）；拒绝（`reject`：记一行、写一条安全审计、什么任务也不改）；被拒投影的列表（`Rejected`）与清理（`JobPurge`、`Purge`：90 天）。
- 不负责：读给用户（`worklist`）；配置的格式（`sources`）。

## 文件

| 文件 | 内容 |
| --- | --- |
| `projections.go` | `contract`（共享任务契约，从 be-protocol 模块读）、`Subs`、`OnEvent`、`seen`、`reject`、`JobPurge`、`Purge`、`Rejected` |

## 依赖与被依赖

- 依赖：`core`、`sources`。
- 被依赖：入口（订阅、路由、作业）。

## 测试

`itest/worklist_integration_test.go`（按版本覆盖、候选替换）、`itest/publishers_integration_test.go`（发布者核对、形状、被拒的列表与审计、心跳）。
