# core：全组件共用的名字与写法（共用）

每个模块都引用、自己什么业务也不做的东西。

## 负责与不负责

- 负责：组件 ID、配置键与缺省（`WORKFLOW_TASK_SOURCES`、`WORKFLOW_STALE_AFTER`）、资源与操作、事件类型的后缀、被拒的原因、参数（`names.go`）；资源定义 `Definitions()`（没有维度）；授权（`Check`）、校验（`Invalid`、`NotFound`）、游标与页大小（`Cursor`、`FromCursor`、`Limit`）（`rules.go`）；`Base`、`Sources`、`Publishers`（`base.go`）。
- 不负责：任何业务规则。

## 文件

| 文件 | 内容 |
| --- | --- |
| `names.go` | 常量；`Definitions()` |
| `rules.go` | `Check`、`Invalid`、`NotFound`、`Cursor`、`FromCursor`、`Limit` |
| `base.go` | `Base`（运行时、配置的发布方、阈值）；`Sources`、`Publishers` |

## 依赖与被依赖

- 依赖：只有 SDK。
- 被依赖：全部模块与入口。

## 测试

入口的单元测试与全部流程测试间接覆盖。
