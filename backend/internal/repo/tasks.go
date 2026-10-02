package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	besdk "github.com/brickKit/be-sdk-go"
)

// 状态与类型常量——字符串字面量与迁移里的 CHECK 约束逐字对应。
const (
	TypeApproval  = "APPROVAL"
	TypeException = "EXCEPTION"

	StatusPending   = "PENDING"
	StatusApproved  = "APPROVED"
	StatusRejected  = "REJECTED"
	StatusResolved  = "RESOLVED"
	StatusCancelled = "CANCELLED"

	ActionApproved  = "APPROVED"
	ActionRejected  = "REJECTED"
	ActionClosed    = "CLOSED"
	ActionCancelled = "CANCELLED"
)

// Task 是待办主体。Summary 保留原始 JSON 字节，不在这一层反解：它是调用方
// 在 CreateTask 时给的展示快照，本组件只透传、不理解它的内容。
type Task struct {
	ID               int64
	Type             string
	Status           string
	AssigneeSub      string
	AssigneeDeptPath string
	Title            string
	Summary          json.RawMessage
	SourceComponent  string
	SourceAggregate  string
	SourceID         string
	DeepLink         string
	DueAt            *time.Time
	Version          int64
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type CreateTaskInput struct {
	IdempotencyKey   string
	Type             string
	AssigneeSub      string
	AssigneeDeptPath string
	Title            string
	Summary          json.RawMessage
	SourceComponent  string
	SourceAggregate  string
	SourceID         string
	DeepLink         string
	DueAt            *time.Time
}

// InScope 判断这条待办对某个调用者是否可见：assignee_dept_path 前缀命中
// （org 维）或 assignee_sub 精确命中（owner 维）任一为真就算可见。用于单条
// 待办（GetTaskDetail：已经拿到 id、判断这个人能不能看）；"我的待办"列表在
// SQL 里用同一个 OR 判据（见 ListInput）。两个操作数都来自调用者自己的
// ScopeOf：dept_path 为空的人（坐在部门树根节点）前缀匹配一切，是 SDK 定义
// 的"不限"，不是漏填。
func (t *Task) InScope(scopePrefix, scopeOwner string) bool {
	return strings.HasPrefix(t.AssigneeDeptPath, scopePrefix) || t.AssigneeSub == scopeOwner
}

func taskIDString(id int64) string { return strconv.FormatInt(id, 10) }

func parseTaskID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: task_id 不合法：%q", ErrInvalidArgument, s)
	}
	return id, nil
}

// CreateTask 登记一条待办，claim-first 幂等（见 claimIdempotency）。事件
// infra.workflow.task.created.v1 与写入在同一个事务里进 Outbox：写成功就一定
// 有事件，回滚就一定没有。
func (r *Repo) CreateTask(ctx context.Context, in CreateTaskInput) (*Task, error) {
	var taskID int64
	err := besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		claimed, err := claimIdempotency(ctx, tx, in.IdempotencyKey, "CreateTask")
		if err != nil {
			return err
		}
		if !claimed {
			resultID, err := replayResult(ctx, tx, in.IdempotencyKey, "CreateTask")
			if err != nil {
				return err
			}
			taskID, err = parseTaskID(resultID)
			return err
		}

		// summary_json 在契约里可选：gRPC 不填时到这里是空字节串（不是 nil），
		// 存成 {}，不把空串塞进 JSONB 列。
		summary := in.Summary
		if len(summary) == 0 {
			summary = json.RawMessage(`{}`)
		}
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO workflow_tasks
				(type, assignee_sub, assignee_dept_path, title, summary,
				 source_component, source_aggregate, source_id, deep_link, due_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			RETURNING id`,
			in.Type, in.AssigneeSub, in.AssigneeDeptPath, in.Title, []byte(summary),
			in.SourceComponent, in.SourceAggregate, in.SourceID, in.DeepLink, in.DueAt,
		).Scan(&taskID); err != nil {
			return fmt.Errorf("写 workflow_tasks: %w", err)
		}

		payload, err := json.Marshal(map[string]any{
			"task_id":          taskIDString(taskID),
			"assignee_sub":     in.AssigneeSub,
			"title":            in.Title,
			"type":             in.Type,
			"source_component": in.SourceComponent,
			"source_aggregate": in.SourceAggregate,
			"source_id":        in.SourceID,
		})
		if err != nil {
			return err
		}
		if err := besdk.PublishOutbox(tx, r.schema, besdk.Event{
			Subject: "infra.workflow.task.created.v1", AggregateID: taskIDString(taskID),
			Version: 1, Payload: payload,
		}); err != nil {
			return err
		}

		return finalizeIdempotency(ctx, tx, in.IdempotencyKey, taskIDString(taskID))
	})
	if err != nil {
		return nil, wrap("建待办", err)
	}
	return r.GetTask(ctx, taskIDString(taskID))
}

// GetTask 按 task_id 查详情，不做数据范围过滤：repo 层只管"这行存不存在"，
// "该不该被这个人看见"由 service 层按 ScopeOf 判断（gRPC 的组件间调用没有
// 用户身份，同样走这里）。
func (r *Repo) GetTask(ctx context.Context, taskID string) (*Task, error) {
	id, err := parseTaskID(taskID)
	if err != nil {
		return nil, err
	}
	var t Task
	err = besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		return scanTask(tx.QueryRowContext(ctx, taskSelectColumns+` FROM workflow_tasks WHERE id = $1`, id), &t)
	})
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: task id=%s", ErrNotFound, taskID)
	}
	if err != nil {
		return nil, wrap("查待办", err)
	}
	return &t, nil
}

// BatchGetTasks 是给调用方防 N+1 的批量读：一次请求按一组 id 取回。查不到的
// id 直接在结果里省略，不报错。
func (r *Repo) BatchGetTasks(ctx context.Context, taskIDs []string) ([]*Task, error) {
	if len(taskIDs) == 0 {
		return nil, nil
	}
	ids := make([]int64, 0, len(taskIDs))
	for _, s := range taskIDs {
		id, err := parseTaskID(s)
		if err != nil {
			continue // 不合法的 id 当"查不到"处理，不报错
		}
		ids = append(ids, id)
	}
	var out []*Task
	err := besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, taskSelectColumns+` FROM workflow_tasks WHERE id = ANY($1::bigint[])`, ids)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t Task
			if err := scanTask(rows, &t); err != nil {
				return err
			}
			out = append(out, &t)
		}
		return rows.Err()
	})
	return out, wrap("批量查待办", err)
}

// ListInput 是 ListTasks 的查询参数。ScopePrefix / ScopeOwner 由 service 层
// 从 besdk.ScopeOf(ctx) 取真实值传入：repo 层不认识 JWT，只认识两个字符串。
type ListInput struct {
	Type            string // 空 = 不筛
	Status          string // 空 = 不筛
	SourceComponent string // 空 = 不筛
	// GET /infra/workflow/tasks（"我的待办"，AdminView=false）：assignee_sub
	// = ScopeOwner OR assignee_dept_path 前缀匹配 ScopePrefix。OR 是契约写明的
	// （"只看得到指派给自己、或指派给自己下属部门某人的待办"），与 Task.InScope
	// 是同一个判据。两个操作数都必须来自调用者真实的 ScopeOf：任何一个留空，
	// 前缀匹配退化成"匹配一切"，整个 OR 就是人人看到全部。
	//
	// GET /infra/workflow/admin/tasks（AdminView=true）：不判 ScopeOwner，只判
	// ScopePrefix——绕过 owner 维、不绕过 org 维，管理员看的是自己部门范围内
	// 的全部待办，不是全租户；可选再叠加 AssigneeSub（管理员显式指定要看的
	// 某个人，与恒等于调用者自己的 ScopeOwner 不是一回事）。gRPC 的 ListTasks
	// 也走这一支、ScopePrefix 留空：组件间调用没有用户身份，看全部。
	AdminView   bool
	ScopeOwner  string // AdminView=false 时用，来自 besdk.ScopeOf(ctx).Owner
	ScopePrefix string // 两种视图都用，来自 besdk.ScopeOf(ctx).Prefix
	AssigneeSub string // 仅 AdminView=true 时可能非空：管理员的显式过滤
	Cursor      string
	PageSize    int32
}

const taskSelectColumns = `SELECT id, type, status, assignee_sub, assignee_dept_path, title, summary,
	source_component, source_aggregate, source_id, deep_link, due_at, version, created_at, updated_at`

// rowScanner 是 *sql.Row 与 *sql.Rows 共有的 Scan：单条查询与列表查询用
// 同一个 scanTask，列的顺序只在 taskSelectColumns 与这里各写一次。
type rowScanner interface {
	Scan(dest ...any) error
}

func scanTask(row rowScanner, t *Task) error {
	var dueAt sql.NullTime
	var summary []byte
	if err := row.Scan(&t.ID, &t.Type, &t.Status, &t.AssigneeSub, &t.AssigneeDeptPath, &t.Title, &summary,
		&t.SourceComponent, &t.SourceAggregate, &t.SourceID, &t.DeepLink, &dueAt, &t.Version,
		&t.CreatedAt, &t.UpdatedAt); err != nil {
		return err
	}
	t.Summary = summary
	if dueAt.Valid {
		t.DueAt = &dueAt.Time
	}
	return nil
}

// ListTasks 按类型 / 状态 / 来源与数据范围筛，按 id 游标分页。ScopePrefix
// 为空是"查全部"（坐在部门树根节点的人看得到全部），是前缀匹配的自然结果，
// 不是 bug；所以 service 层绝不能在 REST 路径上把它留空。
func (r *Repo) ListTasks(ctx context.Context, in ListInput) ([]*Task, string, error) {
	pageSize := in.PageSize
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 50
	}
	var afterID int64
	if in.Cursor != "" {
		id, err := parseTaskID(in.Cursor)
		if err != nil {
			return nil, "", err
		}
		afterID = id
	}

	query := taskSelectColumns + ` FROM workflow_tasks WHERE id > $1`
	args := []any{afterID}
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if in.Type != "" {
		query += ` AND type = ` + arg(in.Type)
	}
	if in.Status != "" {
		query += ` AND status = ` + arg(in.Status)
	}
	if in.SourceComponent != "" {
		query += ` AND source_component = ` + arg(in.SourceComponent)
	}
	// AdminView 决定判不判 ScopeOwner（见 ListInput 字段注释）。LIKE 的 '%'
	// 拼在 SQL 里、不拼进参数值：参数只是调用者的 dept_path 原文。
	if in.AdminView {
		query += ` AND assignee_dept_path LIKE ` + arg(in.ScopePrefix) + ` || '%'`
		if in.AssigneeSub != "" {
			query += ` AND assignee_sub = ` + arg(in.AssigneeSub)
		}
	} else {
		query += ` AND (assignee_sub = ` + arg(in.ScopeOwner) +
			` OR assignee_dept_path LIKE ` + arg(in.ScopePrefix) + ` || '%')`
	}
	query += ` ORDER BY id LIMIT ` + arg(int64(pageSize)+1)

	var out []*Task
	err := besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t Task
			if err := scanTask(rows, &t); err != nil {
				return err
			}
			out = append(out, &t)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, "", wrap("列待办", err)
	}

	nextCursor := ""
	if int32(len(out)) > pageSize {
		out = out[:pageSize]
		nextCursor = taskIDString(out[len(out)-1].ID)
	}
	return out, nextCursor, nil
}
