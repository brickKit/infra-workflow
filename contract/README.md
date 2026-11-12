# infra/workflow 的契约

组件 `infra/workflow`（统一待办）对外发布的契约，随组件的版本发布：

| 文件 | 内容 |
| --- | --- |
| `openapi.yaml` | 用户面：我的待办、已办、读取一个任务（带动作入口与“可能不是最新”）、被拒的投影；每个路由的操作（`x-operation`） |

本组件不发布事件，也不拥有共享任务契约：任务投影的 schema 在线协议 `be-protocol` 的 `schemas/flow/task.v1.schema.json`（流程内核在 SDK 里按它发布），本组件按它消费。系统面没有本组件自己的服务（整套恢复的核对是 SDK 的标准服务）。
