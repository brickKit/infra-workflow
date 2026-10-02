package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	besdk "github.com/brickKit/be-sdk-go"
)

// OverdueBatchSize 是一次超期扫描最多处理的条数；调用方拿返回值与它比，
// 判断要不要立刻再扫一轮。
const OverdueBatchSize = 100

// MarkOverdueAndPublish 扫一批 due_at 已过、仍是 PENDING、还没发过超期
// 事件的待办，标记 overdue_notified_at 并发 infra.workflow.task.overdue.v1。
// 本组件只报告"超期了"，怎么处理（升级、催办、自动通过）是业务组件或通知
// 中心的事——这里绝不修改 status：超期的待办仍是 PENDING，只是多了一条事件。
//
// FOR UPDATE SKIP LOCKED 认领：多个副本（或外壳与独立部署同时在跑）一起扫时，
// 同一条待办只会被一个事务锁住、只发一次事件，别的事务跳过它扫下一批。
// 返回本轮实际处理的条数。

func (r *Repo) MarkOverdueAndPublish(ctx context.Context) (int, error) {
	var count int
	err := besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT id, assignee_sub, due_at FROM workflow_tasks
			WHERE status = 'PENDING' AND due_at IS NOT NULL AND due_at <= now() AND overdue_notified_at IS NULL
			ORDER BY due_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED`, OverdueBatchSize)
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
