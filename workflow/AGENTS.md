# workflow/：入口与组装

组件对外的一个包：`main.go` 与外壳只引用它（`workflow.Definition()`、`workflow.New`）。这里只**组装**：把 `internal/` 下业务模块的函数登记成 SDK 要的路由、订阅、作业与恢复核对；业务一行也不写。

## 负责与不负责

- 负责：组件的身份与版本、自己的配置键（`WORKFLOW_TASK_SOURCES`、`WORKFLOW_STALE_AFTER`）、迁移、把哪个路由交给哪个模块、按配置建哪些订阅。
- 不负责：业务规则（去 [internal/AGENTS.md](../internal/AGENTS.md) 找模块）；名字（`internal/core/names.go`）。

## 文件

| 文件 | 内容 |
| --- | --- |
| `definition.go` | `Version`、端口、迁移、`Keys` 与 `Own`（配置键与清单里的说明）；`Definition()` |
| `module.go` | `Module` 与 `New`（读配置：格式不对成员不启动）；`Routes`（四条用户面路由）、`Subscriptions`（配置里每个发布方的流 → `projections.OnEvent`）、`Jobs`（被拒投影的清理） |
| `checks.go` | `RecoveryChecks`：没有自己的检查（任务是投影，补发重建；SDK 自带的检查覆盖消费进度） |
| `contract_test.go` | 单元测试：清单的 `configSchema` 与 `events`、OpenAPI 与路由一一对应、合并安全 |

迁移在 `migrations/`。

## 依赖与被依赖

- 依赖：`internal/core`、`internal/projections`、`internal/sources`、`internal/worklist`。
- 被依赖：`main.go`、外壳、`itest/`。

## 测试

`go test ./workflow/`。
