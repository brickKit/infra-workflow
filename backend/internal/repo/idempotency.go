package repo

import (
	"context"
	"database/sql"
	"fmt"
)

// claimIdempotency 尝试声明一个写命令——claim-first（同 erp-inventory
// 的既有先例，比"先查后插"强）：先原子 INSERT ... ON CONFLICT DO
// NOTHING 声明，声明成功（claimed=true）才做真正的写；声明失败说明
// 这个 idempotency_key 已经被处理过（或正在被并发的另一个请求处理，
// 那个请求会阻塞在同一行的写锁上直到先到的事务提交/回滚，不会出现
// 两边都"以为自己是第一次"的窗口）。
func claimIdempotency(ctx context.Context, tx *sql.Tx, key, command string) (claimed bool, err error) {
	res, err := tx.ExecContext(ctx,
		`INSERT INTO command_idempotency (idempotency_key, command) VALUES ($1, $2)
		 ON CONFLICT (idempotency_key) DO NOTHING`,
		key, command)
	if err != nil {
		return false, fmt.Errorf("声明 command_idempotency: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func finalizeIdempotency(ctx context.Context, tx *sql.Tx, key, resultID string) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE command_idempotency SET result_id = $1 WHERE idempotency_key = $2`, resultID, key)
	if err != nil {
		return fmt.Errorf("落地 command_idempotency 结果: %w", err)
	}
	return nil
}

func lookupIdempotencyResult(ctx context.Context, tx *sql.Tx, key string) (string, error) {
	var resultID string
	if err := tx.QueryRowContext(ctx,
		`SELECT result_id FROM command_idempotency WHERE idempotency_key = $1`, key).Scan(&resultID); err != nil {
		return "", fmt.Errorf("查 command_idempotency: %w", err)
	}
	return resultID, nil
}
