# infra-workflow

组件 `infra/workflow`（统一待办）的仓库：把部署选定的流程管理方按共享任务契约发布的任务投影汇总起来，给每个人看“我的待办”和“已办”，并给出每个任务的动作入口（所属组件）。用法与部署在 `BRICKKIT.md`，设计在 `docs/design.md`，代码地图在 `AGENTS.md`。

旧版本（2.x）在分支 `legacy/v2`，标签保留；本主分支按终局设计从零开始（D59），版本从 `3.0.0` 起（A110）。设计依据：装配项目 [be-assembly-standard](https://github.com/brickKit/be-assembly-standard) 的边界卡 `docs/zh/07-components/infra.md`（`infra/workflow` 一节）与协议 09。

## 在项目里使用

在装配项目里 `brickkit add infra/workflow@<版本>`，再按 `BRICKKIT.md` 的“部署前准备”配置（`WORKFLOW_TASK_SOURCES` 列出每种任务类型的发布方）。

## 文档

| 问题 | 看哪里 |
| --- | --- |
| 它做什么、怎样部署与配置 | `BRICKKIT.md` |
| 为什么这样设计：术语、数据、规则、未决问题 | `docs/design.md` |
| 怎样改它 | `AGENTS.md` |
| 契约 | `contract/README.md` |

## 开发

见 `AGENTS.md` 的“构建与测试”。
