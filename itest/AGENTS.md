# itest/：集成测试与测试索引

统一待办对着真实的 PostgreSQL 与消息系统、经它真实的接口的测试（L2）。构建标签 `integration`；一条业务流程一个文件。

## 怎样跑

- `make test-integration`（`go test -count=1 -tags integration ./itest/`）；环境变量 `BE_SDK_TEST_PG_ADMIN`、`BE_SDK_TEST_NATS_URL`（装配项目的 `itenv.sh`）。
- 测试每次删掉再建 schema `infra_workflow_it`；删掉再建测试自己的流 `test.buy`、`test.pay`、`test.old`（不碰任何真实组件的流）；端口 28201、9201 要空着。
- 流程管理方由测试自己扮演：在测试组件 `test/buy`、`test/pay`、`test/old` 的流上发投影与心跳，信封的来源就是那个组件。
- **只有一个测试** `TestWorkflowEndToEnd`，按下表的顺序跑各步。

## 文件

| 文件 | 步 | 测的流程 | 规则 |
| --- | --- | --- | --- |
| `worklist_integration_test.go` | `worklist`、`done` | 我的待办（暂停的不在任何人的待办里、没有授予被拒）、筛选、两页与最后一页、坏游标；读取一个任务带动作入口、别人 404、组件不对 404、缺组件 400；新版本换候选、旧版本不覆盖；已办（决定的与曾是候选的，最近的在前）、曾是候选仍能读 | WF02–WF05、WF07 |
| `publishers_integration_test.go` | `publisherCheck`、`staleness`、`recoveryChecks` | 冒用别人的任务类型、没配的类型、不合契约、组件对不上来源、在自己的流上冒充别人的来源、身份不是任务身份都被拒、不建任务、列给管理员、各有安全审计；以前的发布方只为它已有的任务；发布方久无动静标“可能不是最新”、仍在待办里、心跳清除；恢复核对只许恢复工具 | WF01、WF06、WF08、WF09；线协议第 14 章 |

共用的：

| 文件 | 内容 |
| --- | --- |
| `endtoend_integration_test.go` | `TestWorkflowEndToEnd`；`fixture`（运行中的成员、Alice 管理、Bob 与 Carol 只有待办、Dave 没有授予）；`start`；发布方一侧的 `projection`、`publish`、`publishAs`（冒充来源）、`send`、`task`（发并等到这一版）；`client.do`、`taskIDs`、`portFree` |

## 加测试

新流程加一个文件，在上表加一行；测试不为了通过而改。
