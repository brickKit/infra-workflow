# sources：部署选定的发布方

`WORKFLOW_TASK_SOURCES` 的格式、它推出的订阅、发布者核对（`docs/design.md` 的 WF01、WF08）。纯函数，没有表。

## 负责与不负责

- 负责：`Parse`（格式；格式不对、任务类型写两次是错误，成员不启动）、`Components`（配置里出现的每个发布方，现在的与以前的）、`Stream` 与 `ComponentOf`（组件身份与流名互推，线协议 P17.15）、`Judge`（一份投影能不能收：类型没配、不是选定的发布方、以前的发布方的新任务都拒绝）。
- 不负责：收投影与写表（`projections`）。

## 文件

| 文件 | 内容 |
| --- | --- |
| `sources.go` | `Parse`、`Components`、`Stream`、`ComponentOf`、`Judge` |
| `sources_test.go` | 单元测试：配置的格式（含各种错误）、发布者核对的每种情形 |

## 依赖与被依赖

- 依赖：`core`。
- 被依赖：`projections`；入口（`New` 读配置）。

## 测试

`sources_test.go`；集成：`itest/publishers_integration_test.go`。
