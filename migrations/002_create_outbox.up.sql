-- event_outbox：所有组件都有这张表，按 created_at 周分区（§11.2.5）。
-- ⚠️ 本组件没有 event_inbox——设计计划 §4 消费表明确"一条都不消费"
-- （§6.6 铁律三的直接推论：消费任何业务事件都等于在建立"哪个事件
-- 对应关掉哪条待办"的映射，那就是业务规则）。同 infra-authz 的既有
-- 判据：YAGNI，不为不存在的消费面建表。
--
-- ⚠️ 初始分区覆盖当前周起 4 周（迁移执行时是 2026-09-09 那一周）。
-- 其余分区由组件内置定时任务自动建（决策 54、§11.5.1）——这里只需要
-- 保证迁移跑完那一刻起系统能正常写入，不需要预先建满未来所有分区。
CREATE TABLE event_outbox (
    id           BIGSERIAL,
    subject      TEXT        NOT NULL,
    aggregate_id TEXT        NOT NULL,
    version      BIGINT      NOT NULL,
    trace_id     TEXT        NOT NULL DEFAULT '',
    causation_id TEXT        NOT NULL DEFAULT '',
    hop_count    INT         NOT NULL DEFAULT 0,
    payload      JSONB       NOT NULL,
    published_at TIMESTAMPTZ,
    attempts     INT         NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    status       TEXT        NOT NULL DEFAULT 'PENDING',
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);
CREATE TABLE event_outbox_2026_09_07 PARTITION OF event_outbox
  FOR VALUES FROM ('2026-09-07') TO ('2026-09-14');
CREATE TABLE event_outbox_2026_09_14 PARTITION OF event_outbox
  FOR VALUES FROM ('2026-09-14') TO ('2026-09-21');
CREATE TABLE event_outbox_2026_09_21 PARTITION OF event_outbox
  FOR VALUES FROM ('2026-09-21') TO ('2026-09-28');
CREATE TABLE event_outbox_2026_09_28 PARTITION OF event_outbox
  FOR VALUES FROM ('2026-09-28') TO ('2026-10-05');
CREATE INDEX event_outbox_pending ON event_outbox (status, created_at)
  WHERE status = 'PENDING';

-- ⚠️ 实测踩坑（同其余组件的 002 迁移）：建分区要求执行者是父表的
-- owner，迁移用管理凭据跑，建出来的表默认属于那个账号；分区维护后台
-- 任务运行时用 infra_workflow_rw（SET LOCAL ROLE 切换）建未来的分区，
-- 两者不是同一身份，必须显式把 owner 转过去。
ALTER TABLE event_outbox OWNER TO infra_workflow_rw;
