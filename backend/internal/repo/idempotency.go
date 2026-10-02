package repo

import (
	"context"
	"database/sql"
	"fmt"
)

// claimIdempotency 尝试声明一个写命令（claim-first）：先原子地
// INSERT ... ON CONFLICT DO NOTHING 声明，声明成功（claimed=true）才做真正的
// 写；声明失败说明这个 idempotency_key 已经被处理过。并发的同一个 key 会阻塞
// 在同一行的写锁上，直到先到的事务提交或回滚，不会两边都"以为自己是第一次"。
// "先查再插"在并发下有窗口，两个同 key 的请求可能各建一条待办。
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

// replayResult 是声明失败（这个 key 已经有人用过）时取上一次结果的地方：
// 只有同一个命令的重放才算幂等。key 被另一个命令用过（调用方拼 key 时撞了）
// 就报参数错误——照样短路的话，CloseTask 会把 CreateTask 的结果当成"已经
// 关闭过"返回，待办其实还是 PENDING，调用方却以为关掉了。
func replayResult(ctx context.Context, tx *sql.Tx, key, command string) (string, error) {
	var stored, resultID string
	if err := tx.QueryRowContext(ctx,
		`SELECT command, result_id FROM command_idempotency WHERE idempotency_key = $1`, key).Scan(&stored, &resultID); err != nil {
		return "", fmt.Errorf("查 command_idempotency: %w", err)
	}
	if stored != command {
		return "", fmt.Errorf("%w: idempotency_key %q 已经被 %s 用过，不能再用于 %s", ErrInvalidArgument, key, stored, command)
	}
	return resultID, nil
}
