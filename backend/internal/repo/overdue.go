package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	besdk "github.com/brickKit/be-sdk-go"
)

// MarkOverdueAndPublish 扫一批 due_at 已过、仍是 PENDING、还没发过超期
// 事件的待办，标记 overdue_notified_at 并发 infra.workflow.task.
// overdue.v1（设计计划 §2、§4）。⚠️ 本组件只报告"超期了"，怎么处理
// （升级/催办/自动通过）归业务组件或 infra-notification（§6.6 铁律一）
// ——这里绝不修改 status，PENDING 待办超期之后仍然是 PENDING，只是
// 多了一条事件。
//
// 一批最多处理 100 条（同 outbox pump 的既有节流判据），返回本轮实际
// 处理的条数——调用方（module.go 的后台循环）据此决定要不要立刻再
// 跑一轮而不是等下一个 tick（一次扫描不完时快速追上，见 module.go）。
func (r *Repo) MarkOverdueAndPublish(ctx context.Context) (int, error) {
	var count int
	err := besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT id, assignee_sub, due_at FROM workflow_tasks
			WHERE status = 'PENDING' AND due_at IS NOT NULL AND due_at <= now() AND overdue_notified_at IS NULL
			ORDER BY due_at
			LIMIT 100
			FOR UPDATE SKIP LOCKED`)
		if err != nil {
			return err
		}
		type candidate struct {
			id          int64
			assigneeSub string
			dueAt       sql.NullTime
		}
		var batch []candidate
		for rows.Next() {
			var c candidate
			if err := rows.Scan(&c.id, &c.assigneeSub, &c.dueAt); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		for _, c := range batch {
			if _, err := tx.ExecContext(ctx,
				`UPDATE workflow_tasks SET overdue_notified_at = now() WHERE id = $1`, c.id); err != nil {
				return fmt.Errorf("标记超期: %w", err)
			}
			payload, err := json.Marshal(map[string]any{
				"task_id":      taskIDString(c.id),
				"assignee_sub": c.assigneeSub,
				"due_at":       c.dueAt.Time,
			})
			if err != nil {
				return err
			}
			if err := besdk.PublishOutbox(tx, r.schema, besdk.Event{
				Subject: "infra.workflow.task.overdue.v1", AggregateID: taskIDString(c.id),
				Version: 1, Payload: payload,
			}); err != nil {
				return err
			}
		}
		count = len(batch)
		return nil
	})
	return count, wrap("扫描超期待办", err)
}
