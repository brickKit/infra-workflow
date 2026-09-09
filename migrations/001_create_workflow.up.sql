-- workflow_tasks：待办主体（设计计划 §2）。不分区——待办只在"需要人
-- 介入"时产生，量级远低于交易流水；这条判断有前提（客户把审批开到
-- "每张单都要审"时量级会逼近订单量），已记进设计计划 §9 第 2 条，
-- 留给真实数据出来后复核，不是这里可以随便改的假设。
--
-- ⚠️ assignee_dept_path 是"被指派人的部门"，不是"单据所属部门"——
-- 待办的可见性跟着人走，不跟着单据走（assembly.yaml 的 data_scopes
-- 与这里保持同一个判据，写反的症状是部门主管看不到下属手上跨部门
-- 单据的待办）。
--
-- ⚠️ due_at 是查证 Odoo mail.activity（date_deadline 是一等字段）之后
-- 补的，不是可选装饰——见设计计划 §2 的 ⭐ 警告。
CREATE TABLE workflow_tasks (
    id                 BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    type               TEXT        NOT NULL CHECK (type IN ('APPROVAL', 'EXCEPTION')),
    status             TEXT        NOT NULL DEFAULT 'PENDING'
                                    CHECK (status IN ('PENDING', 'APPROVED', 'REJECTED', 'RESOLVED', 'CANCELLED')),
    assignee_sub       TEXT        NOT NULL,
    assignee_dept_path TEXT        NOT NULL DEFAULT '',
    title              TEXT        NOT NULL,
    -- 展示快照：调用方在 CreateTask 时传入的原始 JSON，原样存原样透传
    -- （设计计划 §1.2 第三行：不 join 业务库）。这里用 JSONB 是安全的
    -- ——内容永远是本组件自己的 gRPC/HTTP 绑定产出的合法 JSON，不是
    -- 外部系统的原始回调体（同 infra-iam-casdoor 那次 payload 该用
    -- TEXT 还是 JSONB 的教训对比：那边的输入不可信，这里可信）。
    summary            JSONB       NOT NULL DEFAULT '{}',
    -- 来源四元组：这条待办是哪个业务组件的哪张单据产生的。
    source_component   TEXT        NOT NULL,
    source_aggregate   TEXT        NOT NULL,
    source_id          TEXT        NOT NULL,
    deep_link          TEXT        NOT NULL DEFAULT '',
    due_at             TIMESTAMPTZ,
    overdue_notified_at TIMESTAMPTZ, -- 超期事件只发一次的去重标记，见 backend/internal/service 的超期扫描
    -- 每次状态流转 +1，同时充当事件信封的 version（同 mdm-customer 的
    -- 既有判据：aggregate 自己的乐观锁版本号直接复用成事件 version，
    -- 不另起一套编号）。
    version            INT         NOT NULL DEFAULT 1,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- 数据权限两维：owner=assignee_sub（等值），org=assignee_dept_path（前缀）。
CREATE INDEX workflow_tasks_assignee_sub_idx ON workflow_tasks (assignee_sub);
CREATE INDEX workflow_tasks_assignee_dept_path_idx ON workflow_tasks (assignee_dept_path text_pattern_ops);
-- "我的待办"列表默认只看 PENDING，按 due_at 排序（超期优先）。
CREATE INDEX workflow_tasks_status_due_at_idx ON workflow_tasks (status, due_at) WHERE status = 'PENDING';
-- 超期扫描：PENDING 且 due_at 已过、且还没发过超期事件的那些行。
CREATE INDEX workflow_tasks_overdue_scan_idx ON workflow_tasks (due_at)
  WHERE status = 'PENDING' AND due_at IS NOT NULL AND overdue_notified_at IS NULL;
-- 按来源四元组反查（业务组件自己可能想按 source_id 找回对应待办，
-- 虽然设计计划没有专门开一个 rpc，先建索引不吃亏）。
CREATE INDEX workflow_tasks_source_idx ON workflow_tasks (source_component, source_aggregate, source_id);

-- workflow_task_actions：审批历史，只增不改（设计计划 §2）。
CREATE TABLE workflow_task_actions (
    id         BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    task_id    BIGINT      NOT NULL REFERENCES workflow_tasks(id),
    actor_sub  TEXT        NOT NULL DEFAULT '', -- CloseTask 由业务组件发起时没有真实用户，留空
    action     TEXT        NOT NULL CHECK (action IN ('APPROVED', 'REJECTED', 'CLOSED', 'CANCELLED')),
    comment    TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX workflow_task_actions_task_id_idx ON workflow_task_actions (task_id, created_at);

-- command_idempotency：三个写命令（CreateTask/CloseTask/CancelTask）
-- 统一走这张表的 claim-first 模式（同 erp-inventory 的既有先例：
-- claimIdempotency 先 INSERT ... ON CONFLICT DO NOTHING 声明，声明
-- 成功才做真正的写，写完把 result_id 落回这一行；没声明成功就直接
-- 按 idempotency_key 查 result_id 返回，同一个命令重放多少次都是这
-- 一条逻辑）。⚠️ workflow_tasks 表本身不重复存 idempotency_key——
-- 那不是待办这个资源的持久属性，是命令的输入回执，只属于这张表。
CREATE TABLE command_idempotency (
    idempotency_key TEXT        PRIMARY KEY,
    command         TEXT        NOT NULL,
    result_id       TEXT        NOT NULL DEFAULT '', -- 该命令作用/产生的 task_id，声明阶段先留空
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
