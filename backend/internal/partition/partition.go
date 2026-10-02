// Package partition 是 Module.Start 的后台循环之一：为 event_outbox 自动创建
// 未来的周分区。本组件只有这一张分区表（不消费事件，没有 event_inbox）。
// 分区不存在时写入直接失败，所以要提前建好。
package partition

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	besdk "github.com/brickKit/be-sdk-go"
)

const (
	checkInterval  = 24 * time.Hour
	lookAheadWeeks = 4 // 提前建好当前周 + 未来 4 周，留足缓冲
)

var weeklyPartitionedTables = []string{"event_outbox"}

// Start 立刻检查一次，之后每 24 小时检查一次。单次检查失败只记日志，
// 不让整个循环退出：循环一退，之后就再也没人建分区。
func Start(ctx context.Context, db *sql.DB, role, schema string, logger *slog.Logger) error {
	check := func() {
		err := ensureAllWeekly(ctx, db, role, schema)
		// ctx 已取消是进程在关停，检查半途失败不是故障，不记。
		if err != nil && ctx.Err() == nil {
			logger.Error("周分区维护失败", "error", err)
		}
	}
	check()

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			check()
		}
	}
}

func ensureAllWeekly(ctx context.Context, db *sql.DB, role, schema string) error {
	return besdk.WithTx(ctx, db, role, schema, func(tx *sql.Tx) error {
		weekStart := mondayOf(time.Now().UTC())
		for i := 0; i <= lookAheadWeeks; i++ {
			from := weekStart.AddDate(0, 0, 7*i)
			to := from.AddDate(0, 0, 7)
			for _, table := range weeklyPartitionedTables {
				if err := ensurePartition(ctx, tx, table, from, to); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func mondayOf(t time.Time) time.Time {
	weekday := int(t.Weekday())
	if weekday == 0 {
		weekday = 7
	}
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return d.AddDate(0, 0, -(weekday - 1))
}

// ensurePartition 用 to_regclass 先确认分区存不存在，不存在才建。不能反过来
// "先建、报 already exists 就忽略"：报错会让整个事务进入 aborted 状态，同一
// 事务里后面的分区全都建不了。
func ensurePartition(ctx context.Context, tx *sql.Tx, table string, from, to time.Time) error {
	name := fmt.Sprintf("%s_%s", table, from.Format("2006_01_02"))

	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists); err != nil {
		return fmt.Errorf("检查分区是否存在 %s: %w", name, err)
	}
	if exists {
		return nil
	}

	stmt := fmt.Sprintf(
		`CREATE TABLE %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')`,
		name, table, from.Format("2006-01-02"), to.Format("2006-01-02"),
	)
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("建分区 %s: %w", name, err)
	}
	return nil
}
