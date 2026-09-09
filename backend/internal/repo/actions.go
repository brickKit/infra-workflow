package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	besdk "github.com/brickKit/be-sdk-go"
)

// transitionTaskTx 是 approve/reject/close/cancel 四个动作共用的核心：
// 锁行、校验仍是 PENDING、更新状态、记一条 workflow_task_actions。
// ⚠️ 统一策略：不管走 REST（人点按钮）还是 gRPC（业务组件调
// CloseTask/CancelTask），对一个已经不是 PENDING 的任务再次动作都
// 返回 ErrNotPending——不做"静默忽略"，claim-first 已经在更上一层
// 保证了"同一个 idempotency_key 重放"不会走到这里；真的走到这里却
// 状态不对，说明调用方拿错了 task_id 或者有并发的另一个动作抢先了，
// 两种情况都值得报错而不是悄悄吞掉。
func transitionTaskTx(ctx context.Context, tx *sql.Tx, id int64, newStatus, action, actorSub, comment string) (*Task, error) {
	var t Task
	row := tx.QueryRowContext(ctx, taskSelectColumns+` FROM workflow_tasks WHERE id = $1 FOR UPDATE`, id)
	if err := scanTask(row, &t); err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: task id=%d", ErrNotFound, id)
	} else if err != nil {
		return nil, fmt.Errorf("查 workflow_tasks: %w", err)
	}
	if t.Status != StatusPending {
		return nil, ErrNotPending
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE workflow_tasks SET status = $1, version = version + 1, updated_at = now() WHERE id = $2`,
		newStatus, id); err != nil {
		return nil, fmt.Errorf("更新 workflow_tasks: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workflow_task_actions (task_id, actor_sub, action, comment) VALUES ($1, $2, $3, $4)`,
		id, actorSub, action, comment); err != nil {
		return nil, fmt.Errorf("写 workflow_task_actions: %w", err)
	}
	t.Status = newStatus
	t.Version++
	return &t, nil
}

func publishTaskCompleted(tx *sql.Tx, schema string, t *Task, action, actorSub, comment string) error {
	payload, err := json.Marshal(map[string]any{
		"task_id":          taskIDString(t.ID),
		"action":           action,
		"actor_sub":        actorSub,
		"comment":          comment,
		"source_component": t.SourceComponent,
		"source_aggregate": t.SourceAggregate,
		"source_id":        t.SourceID,
	})
	if err != nil {
		return err
	}
	return besdk.PublishOutbox(tx, schema, besdk.Event{
		Subject: "infra.workflow.task.completed.v1", AggregateID: taskIDString(t.ID),
		Version: t.Version, Payload: payload,
	})
}

// ApproveTask：人点"同意"。REST 面用，没有 idempotency_key——双击/
// 重复提交靠"已经不是 PENDING 就报 409"这条路径本身天然挡住。
func (r *Repo) ApproveTask(ctx context.Context, taskID, actorSub, comment string) (*Task, error) {
	id, err := parseTaskID(taskID)
	if err != nil {
		return nil, err
	}
	var result *Task
	err = besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		t, err := transitionTaskTx(ctx, tx, id, StatusApproved, ActionApproved, actorSub, comment)
		if err != nil {
			return err
		}
		if err := publishTaskCompleted(tx, r.schema, t, "APPROVED", actorSub, comment); err != nil {
			return err
		}
		result = t
		return nil
	})
	return result, wrap("同意待办", err)
}

// RejectTask：人点"驳回"（附言由 http 层的请求体绑定强制要求非空，
// 见设计计划 §3 的 REST 表——那是入参形状校验，不属于这一层的职责）。
func (r *Repo) RejectTask(ctx context.Context, taskID, actorSub, comment string) (*Task, error) {
	id, err := parseTaskID(taskID)
	if err != nil {
		return nil, err
	}
	var result *Task
	err = besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		t, err := transitionTaskTx(ctx, tx, id, StatusRejected, ActionRejected, actorSub, comment)
		if err != nil {
			return err
		}
		if err := publishTaskCompleted(tx, r.schema, t, "REJECTED", actorSub, comment); err != nil {
			return err
		}
		result = t
		return nil
	})
	return result, wrap("驳回待办", err)
}

type CloseTaskInput struct {
	IdempotencyKey string
	TaskID         string
	Comment        string
}

// CloseTask 由业务组件主动关闭 exception 任务——claim-first 幂等
// （设计计划 §1.1、§3.1）。⚠️ task_id 必须由调用方带来，idempotency_key
// 只保证这次关闭命令本身重放安全（同 CloseTaskRequest 的契约注释）。
func (r *Repo) CloseTask(ctx context.Context, in CloseTaskInput) (*Task, error) {
	id, err := parseTaskID(in.TaskID)
	if err != nil {
		return nil, err
	}
	var resultID string
	err = besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		claimed, err := claimIdempotency(ctx, tx, in.IdempotencyKey, "CloseTask")
		if err != nil {
			return err
		}
		if !claimed {
			resultID, err = lookupIdempotencyResult(ctx, tx, in.IdempotencyKey)
			return err
		}
		t, err := transitionTaskTx(ctx, tx, id, StatusResolved, ActionClosed, "", in.Comment)
		if err != nil {
			return err
		}
		// actor_sub 留空——CloseTask 由业务组件发起，没有真实用户
		// （设计计划 §2 的 workflow_task_actions.actor_sub 注释）。
		if err := publishTaskCompleted(tx, r.schema, t, "RESOLVED", "", in.Comment); err != nil {
			return err
		}
		resultID = taskIDString(t.ID)
		return finalizeIdempotency(ctx, tx, in.IdempotencyKey, resultID)
	})
	if err != nil {
		return nil, wrap("关闭待办", err)
	}
	return r.GetTask(ctx, resultID)
}

type CancelTaskInput struct {
	IdempotencyKey string
	TaskID         string
	Reason         string
}

// CancelTask：来源单据作废，待办跟着作废（设计计划 §3）。⚠️ 发的是
// infra.workflow.task.cancelled.v1，不是 task.completed.v1——两条是
// 设计计划 §4 事件表里独立的两条 subject，不能合并。
func (r *Repo) CancelTask(ctx context.Context, in CancelTaskInput) (*Task, error) {
	id, err := parseTaskID(in.TaskID)
	if err != nil {
		return nil, err
	}
	var resultID string
	err = besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		claimed, err := claimIdempotency(ctx, tx, in.IdempotencyKey, "CancelTask")
		if err != nil {
			return err
		}
		if !claimed {
			resultID, err = lookupIdempotencyResult(ctx, tx, in.IdempotencyKey)
			return err
		}
		t, err := transitionTaskTx(ctx, tx, id, StatusCancelled, ActionCancelled, "", in.Reason)
		if err != nil {
			return err
		}
		payload, err := json.Marshal(map[string]any{
			"task_id":          taskIDString(t.ID),
			"reason":           in.Reason,
			"source_component": t.SourceComponent,
			"source_aggregate": t.SourceAggregate,
			"source_id":        t.SourceID,
		})
		if err != nil {
			return err
		}
		if err := besdk.PublishOutbox(tx, r.schema, besdk.Event{
			Subject: "infra.workflow.task.cancelled.v1", AggregateID: taskIDString(t.ID),
			Version: t.Version, Payload: payload,
		}); err != nil {
			return err
		}
		resultID = taskIDString(t.ID)
		return finalizeIdempotency(ctx, tx, in.IdempotencyKey, resultID)
	})
	if err != nil {
		return nil, wrap("作废待办", err)
	}
	return r.GetTask(ctx, resultID)
}

// GetTaskStatus 支持按 task_id 或 idempotency_key 二选一查——超时恰恰
// 是唯一拿不到 task_id 的场景（设计计划 §3.1 ⭐，erp-inventory 阶段二
// 用真机故障注入测试换来的教训）。NOT_FOUND 与 CANCELLED 不许合并
// 成一个"没有"：前者说明请求根本没到（可安全重试），后者说明已被
// 作废（重试是错的）。
func (r *Repo) GetTaskStatus(ctx context.Context, taskID, idempotencyKey string) (status, resolvedTaskID string, err error) {
	if taskID == "" && idempotencyKey == "" {
		return "", "", fmt.Errorf("%w: task_id 与 idempotency_key 不能同时为空", ErrInvalidArgument)
	}
	err = besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		id := taskID
		if id == "" {
			resultID, lookupErr := lookupIdempotencyResult(ctx, tx, idempotencyKey)
			if errors.Is(lookupErr, sql.ErrNoRows) {
				return nil // NOT_FOUND：请求根本没到，可安全重试
			}
			if lookupErr != nil {
				return lookupErr
			}
			if resultID == "" {
				// 已经声明但还没 finalize（同一事务里正在处理，或者
				// claim 之后处理失败——不管哪种，"还没有结果"本身就是
				// NOT_FOUND 的语义，不该报成某个具体状态）。
				return nil
			}
			id = resultID
		}
		numID, parseErr := parseTaskID(id)
		if parseErr != nil {
			return nil // 不合法的 id 当 NOT_FOUND 处理
		}
		var t Task
		row := tx.QueryRowContext(ctx, taskSelectColumns+` FROM workflow_tasks WHERE id = $1`, numID)
		scanErr := scanTask(row, &t)
		if scanErr == sql.ErrNoRows {
			return nil // NOT_FOUND
		}
		if scanErr != nil {
			return scanErr
		}
		status = t.Status
		resolvedTaskID = taskIDString(t.ID)
		return nil
	})
	return status, resolvedTaskID, wrap("查待办状态", err)
}

// TaskAction 是一条审批历史记录，供 GetTask 详情用。
type TaskAction struct {
	ActorSub  string
	Action    string
	Comment   string
	CreatedAt time.Time
}

// ListTaskActions 按时间顺序返回一个待办的全部审批历史（只增不改，
// 设计计划 §2）。
func (r *Repo) ListTaskActions(ctx context.Context, taskID string) ([]TaskAction, error) {
	id, err := parseTaskID(taskID)
	if err != nil {
		return nil, err
	}
	var out []TaskAction
	err = besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT actor_sub, action, comment, created_at FROM workflow_task_actions
			WHERE task_id = $1 ORDER BY created_at`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a TaskAction
			if err := rows.Scan(&a.ActorSub, &a.Action, &a.Comment, &a.CreatedAt); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, wrap("查审批历史", err)
}
