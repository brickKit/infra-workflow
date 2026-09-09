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

// Task 是待办主体。Summary 保留原始 JSON 字节，不在这一层反解——本组件
// 只透传展示快照，不理解它的内容（设计计划 §1.2 第三行）。
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

// InScope 判断这条待办对某个调用者是否可见——assignee_dept_path 前缀
// 命中（org 维）或 assignee_sub 精确命中（owner 维）任一为真就算可见
// （同 erp-sales Order.InScope 的既有判据）。⚠️ 只用于单条待办的可见性
// 校验（GetTaskDetail 这类"已经拿到 id、判断这个人能不能看"的场景），
// 不用于列表查询——"我的待办"列表刻意只用 owner 维精确匹配，不会因为
// 恰好命中 org 维就把下属的待办也列出来（那是 GET /admin/tasks 的职责，
// 见 ListInput.ViewMine 的注释）。
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

// CreateTask 登记一条待办——claim-first 幂等（设计计划 §3.1）。事件
// infra.workflow.task.created.v1 与写入在同一个事务里发布（Outbox
// Pattern，设计计划 §3.10）。
func (r *Repo) CreateTask(ctx context.Context, in CreateTaskInput) (*Task, error) {
	var taskID int64
	err := besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		claimed, err := claimIdempotency(ctx, tx, in.IdempotencyKey, "CreateTask")
		if err != nil {
			return err
		}
		if !claimed {
			resultID, err := lookupIdempotencyResult(ctx, tx, in.IdempotencyKey)
			if err != nil {
				return err
			}
			taskID, err = parseTaskID(resultID)
			return err
		}

		summary := in.Summary
		if summary == nil {
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

// GetTask 按 task_id 查详情。⚠️ 不做数据权限过滤——那是 http 层
// ScopeOf 的责任（同 erp-sales GetOrder 的既有判据：repo 层只管
// "这行存不存在"，调用方决定"这行该不该被这个人看见"）。
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

// BatchGetTasks 是防 N+1 的唯一合法批量读方式（§3.8）。查不到的 id
// 直接在结果里省略，不报错（同 batchGet 惯例）。
func (r *Repo) BatchGetTasks(ctx context.Context, taskIDs []string) ([]*Task, error) {
	if len(taskIDs) == 0 {
		return nil, nil
	}
	ids := make([]int64, 0, len(taskIDs))
	for _, s := range taskIDs {
		id, err := parseTaskID(s)
		if err != nil {
			continue // 不合法的 id 当"查不到"处理，不报错（同 batchGet 惯例）
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
			if err := scanTaskRows(rows, &t); err != nil {
				return err
			}
			out = append(out, &t)
		}
		return rows.Err()
	})
	return out, wrap("批量查待办", err)
}

// ListInput 是 ListTasks 的查询参数。ScopePrefix/ScopeOwner 由调用方
// （service 层）从 besdk.ScopeOf(ctx) 取真实值传入——repo 层不认识
// JWT，只认识两个字符串（同 erp-sales ListInput 的既有判据）。
type ListInput struct {
	Type            string // 空 = 不筛
	Status          string // 空 = 不筛
	SourceComponent string // 空 = 不筛
	// GET /infra/workflow/tasks（"我的待办"，AdminView=false）：assignee_sub
	// = ScopeOwner **OR** assignee_dept_path 前缀匹配 ScopePrefix——两维
	// 都来自调用者自己的 besdk.ScopeOf(ctx)，OR 是刻意的（契约
	// workflow.openapi.yaml 明文："只看得到指派给自己、或指派给自己下属
	// 部门某人的待办"）：看得见自己的，也看得见自己管辖部门内所有人的，
	// 同 Task.InScope 单条校验用的同一个判据，只是这里用在列表查询上。
	//
	// GET /infra/workflow/admin/tasks（AdminView=true）：不判 ScopeOwner，
	// 只判 ScopePrefix（"绕过 owner 维但不绕过 org 维"，管理员看的是本
	// 组织范围内的全部待办，不是全租户）；可选再叠加 AssigneeSub 做进一
	// 步的精确narrow-down（契约里 admin 端点独有的 assignee_sub 查询参数，
	// 这是管理员显式指定要查的某个人，与 ScopeOwner 语义不同——后者恒等
	// 于调用者自己）。
	AdminView   bool
	ScopeOwner  string // AdminView=false 时用，来自 besdk.ScopeOf(ctx).Owner
	ScopePrefix string // 两种视图都用，来自 besdk.ScopeOf(ctx).Prefix
	AssigneeSub string // 仅 AdminView=true 时可能非空：管理员的显式过滤
	Cursor      string
	PageSize    int32
}

const taskSelectColumns = `SELECT id, type, status, assignee_sub, assignee_dept_path, title, summary,
	source_component, source_aggregate, source_id, deep_link, due_at, version, created_at, updated_at`

func scanTask(row *sql.Row, t *Task) error {
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

func scanTaskRows(rows *sql.Rows, t *Task) error {
	var dueAt sql.NullTime
	var summary []byte
	if err := rows.Scan(&t.ID, &t.Type, &t.Status, &t.AssigneeSub, &t.AssigneeDeptPath, &t.Title, &summary,
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

// ListTasks 按 assignee/状态/来源筛，走 ScopeFilter（设计计划 §3）。
// ⚠️ ScopePrefix 为空是"查全部"（站在部门树根节点的人看得到全部，同
// erp-sales order.go 的既有判据），这条不是 bug，是前缀匹配的自然结果——
// 两种视图（AdminView 与否）都成立，因为都会用到 ScopePrefix。
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
	// AdminView 决定判不判 ScopeOwner（见 ListInput 字段注释）。⚠️ LIKE 的
	// '%' 拼在 SQL 里而不是 Go 里拼进参数值——同 erp-sales order.go 的既
	// 有写法，ScopePrefix 为空字符串时 `LIKE '' || '%'` 等价于 `LIKE '%'`，
	// 天然对应"站在部门树根节点的人看得到全部"。
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
			if err := scanTaskRows(rows, &t); err != nil {
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
