# infra/workflow

统一待办组件的代码。用法与边界在 `BRICKKIT.md`，为什么这样设计（术语、数据、规则、未决问题）在 [docs/design.md](docs/design.md)；跨组件的规则在装配项目 be-assembly-standard 的协议 09（审批与统一待办）、协议 02（投影）、协议 20（动作直达所属组件）与决定 D68；任务投影的契约与流程管理方的行为在线协议第 17 章。

## 代码地图

代码按业务模块分层。**先读这一节，再进目录**：每个代码目录都有自己的 `AGENTS.md`（同目录的 `CLAUDE.md` 让 Claude Code 读这个目录的文件时自动带上它），写这一层负责什么、不负责什么、每个文件、依赖谁、测在哪。

| 目录 | 是什么 |
| --- | --- |
| [`workflow/`](workflow/AGENTS.md) | 入口：组件定义与组装（路由、按配置的订阅、作业、恢复核对，转给业务模块）；不写业务。`main.go` 与外壳只引用它 |
| [`internal/`](internal/AGENTS.md) | 业务：下面的模块各一个包；模块之间的依赖表在它的 `AGENTS.md` |
| [`itest/`](itest/AGENTS.md) | 集成测试，按业务流程分文件；它的 `AGENTS.md` 是测试索引 |
| `workflow/migrations/` | 表：任务、候选、发布方、被拒的投影 |
| `cmd/seed/` | 种子（`make seed`）：角色“待办维护”给种子用户，“我的待办”给采购与财务的审批人、采购员与出纳；任务本身来自发布方的种子 |
| `contract/` | 本组件发布的契约：`contract/openapi.yaml`（没有事件、没有 proto） |
| `docs/design.md` | 设计（WF01–WF09） |

业务模块（`internal/` 下）：

| 模块 | 负责 | 拥有的表 |
| --- | --- | --- |
| [`projections`](internal/projections/AGENTS.md) | 收任务投影与心跳：发布者核对、按共享任务契约核对形状、按版本覆盖、候选（现在与曾经）、发布方最后的动静；被拒的投影与它的清理 | `task`、`task_candidate`、`publisher`、`rejected_projection` |
| [`worklist`](internal/worklist/AGENTS.md) | 用户面：我的待办、已办、读取一个任务（动作入口、可能不是最新） | —（只读） |
| [`sources`](internal/sources/AGENTS.md) | 部署的发布方配置：格式、订阅的流与事件类型、发布者核对（纯函数） | — |

共用的：[`core`](internal/core/AGENTS.md)（名字、资源与操作、配置键、错误与校验、游标、`Base`）。

| 想改 | 从这里开始 | 然后 |
| --- | --- | --- |
| 谁的投影能进待办；以前的发布方 | `docs/design.md` 的 WF01、WF08 | `internal/sources` 的 `Judge`、`Parse`；`internal/projections` 的 `OnEvent` |
| 待办与已办里有哪些任务、顺序 | `docs/design.md` 的 WF03、WF04 | `internal/worklist` |
| 可能不是最新 | `docs/design.md` 的 WF06 | `internal/worklist` 的 `cols`、`internal/projections` 的 `seen` |

## 构建与测试

- `go test ./...`（`make test`）：清单的 `configSchema` 与 `events`、OpenAPI 与路由、合并安全（`workflow/contract_test.go`）；配置的格式与发布者核对（`internal/sources/sources_test.go`）。
- `make test-integration`（`go test -tags integration ./itest/`）：真实的 PostgreSQL 与消息系统。
- `brickkit lint`

## 设计取舍

- **只保存投影，不保存决定**：动作由前端直接发给所属组件（D68），待办随下一份投影改变；所以本组件没有写路由，也不会和所属组件的状态不一致地“先关掉”任务。
- **数据范围就是“候选包含我”**：候选由发布方按它的分配与授权解析；本组件不判断别的组件的授权，资源没有维度。
- **订阅由配置推出**：发布方组件的流与事件类型按线协议 P17.15 的命名推出，新发布方上线只改配置。

## 易错点

- 动作入口由发布方组件推出，不取投影里的地址（伪造的投影不能把用户引到别处）。
- 发布者是投影所在的流的属主（消息系统按服务身份限制谁能在哪个流上发布，`projections.Subs` 把它交给 `OnEvent`）；信封的来源与投影里的组件都是发布方自己写的，都要等于它，对不上按 `malformed` 拒绝。
- 不在本地关闭任务：发布方久无动静只标“可能不是最新”。

## 改代码前自查

- 改了版本号吗？每次改动都升版本。

<!-- brickkit:managed:begin lang=zh -->
## BrickKit

这是一个 BrickKit 组件：平台只读 `component.yaml`。它依赖的规则：

- `configSchema` 的键就是代码读的环境变量名。不能用保留名：`COMPONENT_ID`、`COMPONENT_VERSION`、`PORT`、`BRICKKIT_SERVED_MEMBERS`、`BRICKKIT_SERVED_MEMBERS_CONFIG`，以及任何 `*_ENDPOINT`。
- 依赖写精确版本。依赖的地址以 `<ID>_ENDPOINT` 注入；缺席的可选依赖根本没有这个变量，读的时候要带兜底。
- `/healthz` 只查本进程，不查依赖。迁移命令用同一个镜像跑，遇到不认识的参数必须直接失败；`local.migrationCommand` 从源码跑同一条迁移，给 `mode: local` 用。
- `BRICKKIT.md` 会随版本进入每个使用它的项目，在那里是脱离仓库单独读的：跟代码一起改，不放相对链接。
- 发版：改 `metadata.version`，提交、推送，`brickkit release`。`brickkit lint` 会检查清单和这些文档——在项目里、在这个目录下跑，只查这个组件（`--all` 查整个项目）。
- 完整规则在 `brickkit-component` 技能里（装了技能的项目或仓库根目录下的 `.claude/skills/brickkit-component/SKILL.md`；`brickkit skills update` 会装上）；参数问 `brickkit <命令> --help`；BrickKit 自己的文档用 `brickkit docs` 看。
<!-- brickkit:managed:end -->
