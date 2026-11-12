# worklist：我的待办、已办、读取一个任务

用户面的三条读取（`docs/design.md` 的 WF03–WF07）。不写任何表。

## 负责与不负责

- 负责：`Mine`（我是现在的候选、待处理；截止早的在前，没有截止的按收到的时刻；按任务类型、法人筛选；游标）、`Done`（我决定的、与我曾是候选的已结束任务；最近的在前）、`Get`（现在或曾经是候选、或是决定者才能看，否则 404；任务是组件加身份）；每条带动作入口（由发布方组件推出）与可能不是最新（`cols`：待处理、发布方的最后心跳与投影都早于阈值）。
- 不负责：收投影（`projections`）；决定（所属组件，D68）。

## 文件

| 文件 | 内容 |
| --- | --- |
| `worklist.go` | `Task`、`Action`、`cols`、`page`、`answer`、`request`、`Mine`、`Done`、`Get` |

## 依赖与被依赖

- 依赖：`core`。
- 被依赖：入口（路由）。

## 测试

`itest/worklist_integration_test.go`（`worklist`、`done`）、`itest/publishers_integration_test.go`（`staleness`）。
