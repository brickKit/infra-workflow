// Package service 是 http 与 grpc 共用的一层：入参校验，以及 repo 层不做的
// 数据范围判断（谁看得见、谁能处理）。事务、claim-first 幂等、事件发布都在
// repo 层随 SQL 一起做，这一层保持薄。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/brickKit/infra-workflow/v2/backend/internal/repo"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var ErrInvalidArgument = errors.New("参数不合法")

type Service struct {
	repo   *repo.Repo
	logger *slog.Logger
}

func New(r *repo.Repo, logger *slog.Logger) *Service {
	return &Service{repo: r, logger: logger}
}

// logFailure 按错误性质选日志级别：调用方断开或进程关停带来的取消 / 超时记 Warn；
// 映射成 Internal 的是服务端故障，运维要处理，记 ERROR；其余都是调用方自己能纠正
// 的错误（参数不合法、幂等键被别的命令用过、找不到、已经不是 PENDING），SDK 的
// 访问日志与 RED 指标已经记下了状态码，这里只记 Info。
func (s *Service) logFailure(ctx context.Context, msg string, err error, attrs ...any) {
	attrs = append(attrs, "error", err)
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), ctx.Err() != nil:
		s.logger.WarnContext(ctx, msg, attrs...)
	case status.Code(ToStatus(err)) == codes.Internal:
		s.logger.ErrorContext(ctx, msg, attrs...)
	default:
		s.logger.InfoContext(ctx, msg, attrs...)
	}
}

// ── 命令：CreateTask / CloseTask / CancelTask（组件间协议，不进 REST）。
// 调用方是业务组件，没有用户身份；这一层只校验入参形状，不判数据范围。──

func (s *Service) CreateTask(ctx context.Context, in repo.CreateTaskInput) (*repo.Task, error) {
	if in.IdempotencyKey == "" {
		return nil, fmt.Errorf("%w: idempotency_key 不能为空", ErrInvalidArgument)
	}
	if in.Type != repo.TypeApproval && in.Type != repo.TypeException {
		return nil, fmt.Errorf("%w: type 不合法：%q", ErrInvalidArgument, in.Type)
	}
	if in.AssigneeSub == "" {
		return nil, fmt.Errorf("%w: assignee_sub 不能为空", ErrInvalidArgument)
	}
	// assignee_dept_path 是被指派人部门的快照：真实路径（以 / 开头），或空串
	// （没分部门，这条待办只对本人可见）。别的值多半是调用方把自己的
	// ScopeFilter.Prefix 原样传了进来——没分部门时那是哨兵 besdk.NoDeptPath，
	// 写进行里之后，所有没分部门的人的前缀都等于它，彼此看得到对方的待办。
	if in.AssigneeDeptPath != "" && !strings.HasPrefix(in.AssigneeDeptPath, "/") {
		return nil, fmt.Errorf("%w: assignee_dept_path 必须以 / 开头或留空：%q", ErrInvalidArgument, in.AssigneeDeptPath)
	}
	if in.Title == "" {
		return nil, fmt.Errorf("%w: title 不能为空", ErrInvalidArgument)
	}
	if in.SourceComponent == "" || in.SourceAggregate == "" || in.SourceID == "" {
		return nil, fmt.Errorf("%w: 来源四元组（source_component/aggregate/id）不能为空", ErrInvalidArgument)
	}
	// summary 写进 JSONB 列；不是合法 JSON 是调用方的错，不能等数据库报错变成 Internal。
	if len(in.Summary) > 0 && !json.Valid(in.Summary) {
		return nil, fmt.Errorf("%w: summary_json 不是合法的 JSON", ErrInvalidArgument)
	}
	t, err := s.repo.CreateTask(ctx, in)
	if err != nil {
		s.logFailure(ctx, "建待办失败", err, "source_component", in.SourceComponent, "source_id", in.SourceID)
		return nil, err
	}
	return t, nil
}

func (s *Service) CloseTask(ctx context.Context, in repo.CloseTaskInput) (*repo.Task, error) {
	if in.IdempotencyKey == "" {
		return nil, fmt.Errorf("%w: idempotency_key 不能为空", ErrInvalidArgument)
	}
	if in.TaskID == "" {
		return nil, fmt.Errorf("%w: task_id 不能为空", ErrInvalidArgument)
	}
	t, err := s.repo.CloseTask(ctx, in)
	if err != nil {
		s.logFailure(ctx, "关闭待办失败", err, "task_id", in.TaskID)
		return nil, err
	}
	return t, nil
}

func (s *Service) CancelTask(ctx context.Context, in repo.CancelTaskInput) (*repo.Task, error) {
	if in.IdempotencyKey == "" {
		return nil, fmt.Errorf("%w: idempotency_key 不能为空", ErrInvalidArgument)
	}
	if in.TaskID == "" {
		return nil, fmt.Errorf("%w: task_id 不能为空", ErrInvalidArgument)
	}
	t, err := s.repo.CancelTask(ctx, in)
	if err != nil {
		s.logFailure(ctx, "作废待办失败", err, "task_id", in.TaskID)
		return nil, err
	}
	return t, nil
}

// GetTaskStatus 的二选一校验在 repo 层，这一层直接透传。
func (s *Service) GetTaskStatus(ctx context.Context, taskID, idempotencyKey string) (status, resolvedTaskID string, err error) {
	return s.repo.GetTaskStatus(ctx, taskID, idempotencyKey)
}

// BatchGetTasks 是组件间协议（防 N+1）：不经过 besdk.RequirePermission，ctx
// 里没有 Claims，不做数据范围过滤。
func (s *Service) BatchGetTasks(ctx context.Context, taskIDs []string) ([]*repo.Task, error) {
	return s.repo.BatchGetTasks(ctx, taskIDs)
}

// ListTasks 是 gRPC 面的 WorkflowService.ListTasks。同 BatchGetTasks：组件间
// 调用不带用户身份（gRPC 侧不转发、不验 JWT），不做数据范围过滤；在这里调
// besdk.ScopeOf 会 panic。人看的两维过滤是 ListMyTasks / ListTasksAdmin 的事，
// 只在 REST 面生效。"看全部部门"用显式的 AllDepts 表达，不用空前缀：repo
// 收到空前缀会报错，这样 REST 路径上漏填范围不会悄悄变成全部。
func (s *Service) ListTasks(ctx context.Context, in repo.ListInput) ([]*repo.Task, string, error) {
	in.AdminView = true
	in.AllDepts = true
	in.ScopePrefix = ""
	in.ScopeOwner = ""
	return s.repo.ListTasks(ctx, in)
}

// ── 读 + 写：REST 面用，走 besdk.ScopeOf(ctx)（人的操作，ctx 里有验过签的 Claims）──

// checkTaskInScope：先按 id 查出这条待办，再判断调用者是否在它的可见范围内。
// 范围外答的是"不存在"（repo.TaskNotFound，REST 404），与真不存在的 id 连错误
// 信息都一样：答 403 就等于告诉调用者"这个 id 有一条待办，只是你看不到"。
// 动作（approve / reject）不走这里，对不是自己的待办仍答 403（actorInScope）。
func (s *Service) checkTaskInScope(ctx context.Context, taskID string) (*repo.Task, error) {
	t, err := s.repo.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	scope := besdk.ScopeOf(ctx)
	if !t.InScope(scope.Prefix, scope.Owner) {
		return nil, repo.TaskNotFound(taskID)
	}
	return t, nil
}

// GetTaskDetail 是 GET /infra/workflow/tasks/{id} 的核心：详情 + 审批历史。
// 可见范围用 Task.InScope 的 OR 判据（被指派人本人，或其部门的上级都能看）：
// 部门主管从"我的待办"列表里看得到下属的待办，点进详情就不该是 403。
func (s *Service) GetTaskDetail(ctx context.Context, taskID string) (*repo.Task, []repo.TaskAction, error) {
	if taskID == "" {
		return nil, nil, fmt.Errorf("%w: task_id 不能为空", ErrInvalidArgument)
	}
	t, err := s.checkTaskInScope(ctx, taskID)
	if err != nil {
		return nil, nil, err
	}
	actions, err := s.repo.ListTaskActions(ctx, taskID)
	if err != nil {
		return nil, nil, err
	}
	return t, actions, nil
}

// ListMyTasks 是 GET /infra/workflow/tasks（"我的待办"）：被指派人是调用者
// 本人、或在调用者管辖部门内的待办都看得到（OR，契约写明）。两个操作数都从
// ScopeOf 取：没分部门的人 Prefix 是哨兵 besdk.NoDeptPath，org 维落空，只剩
// 指派给本人的；漏填 ScopePrefix 由 repo 报 ErrInvalidArgument。
func (s *Service) ListMyTasks(ctx context.Context, in repo.ListInput) ([]*repo.Task, string, error) {
	in.AdminView = false
	in.AllDepts = false // 人看的视图永远带调用者自己的范围
	scope := besdk.ScopeOf(ctx)
	in.ScopeOwner = scope.Owner
	in.ScopePrefix = scope.Prefix
	return s.repo.ListTasks(ctx, in)
}

// ListTasksAdmin 是 GET /infra/workflow/admin/tasks：管理者视角，绕过 owner 维
// （看得到不是指派给自己的待办）但不绕过 org 维——按调用者自己的部门前缀过滤，
// 看不到范围外部门的待办；没分部门的管理员（哨兵前缀）一条也看不到。
// in.AssigneeSub 若由 http 层的查询参数填好，原样透传做进一步的精确过滤。
func (s *Service) ListTasksAdmin(ctx context.Context, in repo.ListInput) ([]*repo.Task, string, error) {
	in.AdminView = true
	in.AllDepts = false // 管理视图绕过 owner 维，不绕过 org 维
	in.ScopePrefix = besdk.ScopeOf(ctx).Prefix
	return s.repo.ListTasks(ctx, in)
}

// actorInScope 是 ApproveTask / RejectTask 共用的一步：这两个动作要求调用者
// 就是被指派人本人，不是 Task.InScope 的 OR 判据——那是"看得见"，这里是
// "能不能替他做决定"。转办、加签都还没有做，所以严格等值，不接受部门主管
// 代批。
func (s *Service) actorInScope(ctx context.Context, t *repo.Task) (actorSub string, err error) {
	actorSub = besdk.ScopeOf(ctx).Owner
	if t.AssigneeSub != actorSub {
		return "", repo.ErrForbidden
	}
	return actorSub, nil
}

func (s *Service) ApproveTask(ctx context.Context, taskID, comment string) (*repo.Task, error) {
	if taskID == "" {
		return nil, fmt.Errorf("%w: task_id 不能为空", ErrInvalidArgument)
	}
	t, err := s.repo.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	actorSub, err := s.actorInScope(ctx, t)
	if err != nil {
		return nil, err
	}
	return s.repo.ApproveTask(ctx, taskID, actorSub, comment)
}

// RejectTask 的附言必须非空。http 层的请求体绑定已经强制要求，这里再兜一道：
// 不经过那个绑定的调用方（测试、将来别的入口）同样被挡住。
func (s *Service) RejectTask(ctx context.Context, taskID, comment string) (*repo.Task, error) {
	if taskID == "" {
		return nil, fmt.Errorf("%w: task_id 不能为空", ErrInvalidArgument)
	}
	if comment == "" {
		return nil, fmt.Errorf("%w: 驳回必须附带理由", ErrInvalidArgument)
	}
	t, err := s.repo.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	actorSub, err := s.actorInScope(ctx, t)
	if err != nil {
		return nil, err
	}
	return s.repo.RejectTask(ctx, taskID, actorSub, comment)
}

// MarkOverdueAndPublish 供 module.go 的超期扫描循环调用，本层不加逻辑。
func (s *Service) MarkOverdueAndPublish(ctx context.Context) (int, error) {
	return s.repo.MarkOverdueAndPublish(ctx)
}
