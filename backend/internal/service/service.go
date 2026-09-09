// Package service 是 infra-workflow 的业务规则层：入参校验 + 给 http/grpc
// 一个不依赖 repo 内部细节的稳定入口（同 erp-inventory/erp-sales 的判据）。
// 真正的事务、claim-first 幂等、事件发布都在 repo 层随 SQL 一起做，这一层
// 依然薄——它只多做一件 repo 层不该做的事：数据权限的可见性/归属校验
// （repo.GetTask 的注释："不做数据权限过滤——那是 http 层 ScopeOf 的
// 责任"，这一层就是那个"http 层"，gRPC 面的编排也走这里共用同一套）。
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/brickKit/infra-workflow/backend/internal/repo"
)

var ErrInvalidArgument = errors.New("参数不合法")

type Service struct {
	repo   *repo.Repo
	logger *slog.Logger
}

func New(r *repo.Repo, logger *slog.Logger) *Service {
	return &Service{repo: r, logger: logger}
}

// ── 命令：CreateTask/CloseTask/CancelTask（组件间协议，不暴露到 REST，
// 见 workflow.proto 顶部的三条铁律警告——调用方是已验证过自己权限的
// 业务组件，这一层不做数据权限校验，只做入参形状校验）──

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
	if in.Title == "" {
		return nil, fmt.Errorf("%w: title 不能为空", ErrInvalidArgument)
	}
	if in.SourceComponent == "" || in.SourceAggregate == "" || in.SourceID == "" {
		return nil, fmt.Errorf("%w: 来源四元组（source_component/aggregate/id）不能为空", ErrInvalidArgument)
	}
	t, err := s.repo.CreateTask(ctx, in)
	if err != nil {
		s.logger.Error("建待办失败", "source_component", in.SourceComponent, "source_id", in.SourceID, "error", err)
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
		s.logger.Error("关闭待办失败", "task_id", in.TaskID, "error", err)
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
		s.logger.Error("作废待办失败", "task_id", in.TaskID, "error", err)
		return nil, err
	}
	return t, nil
}

// GetTaskStatus 的二选一校验已经在 repo 层做了（同一处判断没必要写两遍），
// 这一层直接透传。
func (s *Service) GetTaskStatus(ctx context.Context, taskID, idempotencyKey string) (status, resolvedTaskID string, err error) {
	return s.repo.GetTaskStatus(ctx, taskID, idempotencyKey)
}

// BatchGetTasks 是组件间协议（防 N+1），不经过 besdk.RequirePermission，
// ctx 里没有 Claims——同 erp-inventory BatchGetBalance 的既有判据，不做
// 数据权限过滤。
func (s *Service) BatchGetTasks(ctx context.Context, taskIDs []string) ([]*repo.Task, error) {
	return s.repo.BatchGetTasks(ctx, taskIDs)
}

// ListTasks 是 gRPC 面的 WorkflowService.ListTasks——同 BatchGetTasks，
// 组件间协议不做数据权限过滤（本项目目前没有任何组件在 gRPC 侧转发/
// 验证 JWT，见 workflow.proto 该 rpc 的注释）。人类操作的两维过滤是
// ListMyTasks/ListTasksAdmin 的职责，只在 REST 面生效。
func (s *Service) ListTasks(ctx context.Context, in repo.ListInput) ([]*repo.Task, string, error) {
	in.AdminView = true // 借用"只判 ScopePrefix"这条分支；ScopePrefix 留空 = 看全部
	return s.repo.ListTasks(ctx, in)
}

// ── 读 + 写：REST 面用，走 besdk.ScopeOf(ctx)（人类操作，有已验签 Claims）──

// checkTaskInScope 是 GetTaskDetail 共用的一步：先按 id 查出这条待办，
// 再判断调用者是否在它的可见范围内（同 erp-sales checkOrderInScope 的
// 既有判据）。
func (s *Service) checkTaskInScope(ctx context.Context, taskID string) (*repo.Task, error) {
	t, err := s.repo.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	scope := besdk.ScopeOf(ctx)
	if !t.InScope(scope.Prefix, scope.Owner) {
		return nil, repo.ErrForbidden
	}
	return t, nil
}

// GetTaskDetail 是 GET /infra/workflow/tasks/{id} 的核心：详情 + 审批
// 历史（设计计划 §3）。⚠️ 可见范围用 Task.InScope 的 OR 判据（assignee
// 本人或其部门的上级都能看）——这条口子是刻意的，见 repo.Task.InScope
// 注释：业务组件可能把这条待办的 deep_link 嵌进它自己的单据页面，单据的
// 部门主管点进去时，"这条待办不是分给他的"不该变成 403。
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

// ListMyTasks 是 GET /infra/workflow/tasks（"我的待办"）：assignee 是
// 调用者本人、或调用者管辖部门内任何人的待办都看得到（OR，同
// repo.ListInput 字段注释与 workflow.openapi.yaml 的契约明文）。
func (s *Service) ListMyTasks(ctx context.Context, in repo.ListInput) ([]*repo.Task, string, error) {
	in.AdminView = false
	scope := besdk.ScopeOf(ctx)
	in.ScopeOwner = scope.Owner
	in.ScopePrefix = scope.Prefix
	return s.repo.ListTasks(ctx, in)
}

// ListTasksAdmin 是 GET /infra/workflow/admin/tasks：管理者视角，绕过
// owner 维（能看到不是指派给自己的待办）但不绕过 org 维——按调用者自己
// 的部门前缀过滤，看不到范围外部门的待办（同 repo.ListInput.AdminView
// 注释）。in.AssigneeSub 若已由调用方（http 层的查询参数）填好，原样
// 透传做进一步精确过滤。
func (s *Service) ListTasksAdmin(ctx context.Context, in repo.ListInput) ([]*repo.Task, string, error) {
	in.AdminView = true
	in.ScopePrefix = besdk.ScopeOf(ctx).Prefix
	return s.repo.ListTasks(ctx, in)
}

// actorInScope 是 ApproveTask/RejectTask 共用的一步：这两个动作要求
// 调用者就是被指派人本人——不是 Task.InScope 的 OR 判据（那是"看得
// 见"，这里是"能不能替他做决定"）。"转办/加签"是设计计划 §9 明确列出
// 的开放问题，本阶段不做，所以严格等值，不接受部门主管代批。
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

// RejectTask 的附言必须非空——http 层的请求体绑定会强制要求（见设计计划
// §3 的 REST 表），这一层只补一道兜底（同一份校验写两遍好过漏一层：
// gRPC 与 REST 若将来共用同一个入口，绑定校验不一定总会经过）。
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

// MarkOverdueAndPublish 供 module.go 的后台循环调用——本层不加任何逻辑，
// 只是让 module 包不必直接认识 repo 包（同其余方法的分层判据）。
func (s *Service) MarkOverdueAndPublish(ctx context.Context) (int, error) {
	return s.repo.MarkOverdueAndPublish(ctx)
}
