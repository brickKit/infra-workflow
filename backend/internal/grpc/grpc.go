// Package grpc 实现 infra.workflow.v1.WorkflowService——内部 gRPC 面
// （§2.1）。HTTP 与 gRPC 共用同一个 service.Service，业务逻辑只写一遍
// （同 erp-inventory/infra-authz 的既有判据）。
package grpc

import (
	"context"
	"strconv"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	workflowv1 "github.com/brickKit/infra-workflow/gen/infra/workflow/v1"

	"github.com/brickKit/infra-workflow/backend/internal/repo"
	"github.com/brickKit/infra-workflow/backend/internal/service"
)

type server struct {
	workflowv1.UnimplementedWorkflowServiceServer
	svc *service.Service
}

// New 构造 gRPC 服务端实现。module.go 用它注册到 grpc.Server。
func New(svc *service.Service) workflowv1.WorkflowServiceServer {
	return &server{svc: svc}
}

func toProtoType(t string) workflowv1.TaskType {
	switch t {
	case repo.TypeApproval:
		return workflowv1.TaskType_TASK_TYPE_APPROVAL
	case repo.TypeException:
		return workflowv1.TaskType_TASK_TYPE_EXCEPTION
	default:
		return workflowv1.TaskType_TASK_TYPE_UNSPECIFIED
	}
}

func fromProtoType(t workflowv1.TaskType) string {
	switch t {
	case workflowv1.TaskType_TASK_TYPE_APPROVAL:
		return repo.TypeApproval
	case workflowv1.TaskType_TASK_TYPE_EXCEPTION:
		return repo.TypeException
	default:
		return ""
	}
}

func toProtoStatus(s string) workflowv1.TaskStatus {
	switch s {
	case repo.StatusPending:
		return workflowv1.TaskStatus_TASK_STATUS_PENDING
	case repo.StatusApproved:
		return workflowv1.TaskStatus_TASK_STATUS_APPROVED
	case repo.StatusRejected:
		return workflowv1.TaskStatus_TASK_STATUS_REJECTED
	case repo.StatusResolved:
		return workflowv1.TaskStatus_TASK_STATUS_RESOLVED
	case repo.StatusCancelled:
		return workflowv1.TaskStatus_TASK_STATUS_CANCELLED
	default:
		// ⚠️ UNSPECIFIED 就是 NOT_FOUND 的信号，不是"忘了填"（同
		// erp-inventory ReservationStatus 的既有判据，设计计划 §3.1）。
		return workflowv1.TaskStatus_TASK_STATUS_UNSPECIFIED
	}
}

func fromProtoStatus(s workflowv1.TaskStatus) string {
	switch s {
	case workflowv1.TaskStatus_TASK_STATUS_PENDING:
		return repo.StatusPending
	case workflowv1.TaskStatus_TASK_STATUS_APPROVED:
		return repo.StatusApproved
	case workflowv1.TaskStatus_TASK_STATUS_REJECTED:
		return repo.StatusRejected
	case workflowv1.TaskStatus_TASK_STATUS_RESOLVED:
		return repo.StatusResolved
	case workflowv1.TaskStatus_TASK_STATUS_CANCELLED:
		return repo.StatusCancelled
	default:
		return ""
	}
}

func toProtoTask(t *repo.Task) *workflowv1.Task {
	if t == nil {
		return nil
	}
	pt := &workflowv1.Task{
		Id: taskIDString(t.ID), Type: toProtoType(t.Type), Status: toProtoStatus(t.Status),
		AssigneeSub: t.AssigneeSub, AssigneeDeptPath: t.AssigneeDeptPath,
		Title: t.Title, SummaryJson: string(t.Summary),
		SourceComponent: t.SourceComponent, SourceAggregate: t.SourceAggregate, SourceId: t.SourceID,
		DeepLink:  t.DeepLink,
		CreatedAt: timestamppb.New(t.CreatedAt), UpdatedAt: timestamppb.New(t.UpdatedAt),
	}
	if t.DueAt != nil {
		pt.DueAt = timestamppb.New(*t.DueAt)
	}
	return pt
}

// taskIDString 与 repo 包内同名私有函数逻辑相同（repo.Task.ID 是
// BIGINT，contracts 一律 string），这里不导出 repo 的私有函数，重新
// 写一遍一行代码比跨包破例导出更简单。
func taskIDString(id int64) string {
	return strconv.FormatInt(id, 10)
}

func (s *server) CreateTask(ctx context.Context, req *workflowv1.CreateTaskRequest) (*workflowv1.Task, error) {
	var dueAt *time.Time
	if req.DueAt != nil {
		t := req.DueAt.AsTime()
		dueAt = &t
	}
	t, err := s.svc.CreateTask(ctx, repo.CreateTaskInput{
		IdempotencyKey: req.IdempotencyKey, Type: fromProtoType(req.Type),
		AssigneeSub: req.AssigneeSub, AssigneeDeptPath: req.AssigneeDeptPath,
		Title: req.Title, Summary: []byte(req.SummaryJson),
		SourceComponent: req.SourceComponent, SourceAggregate: req.SourceAggregate, SourceID: req.SourceId,
		DeepLink: req.DeepLink, DueAt: dueAt,
	})
	if err != nil {
		return nil, service.ToStatus(err)
	}
	return toProtoTask(t), nil
}

func (s *server) CloseTask(ctx context.Context, req *workflowv1.CloseTaskRequest) (*workflowv1.CloseTaskResponse, error) {
	t, err := s.svc.CloseTask(ctx, repo.CloseTaskInput{
		IdempotencyKey: req.IdempotencyKey, TaskID: req.TaskId, Comment: req.Comment,
	})
	if err != nil {
		return nil, service.ToStatus(err)
	}
	return &workflowv1.CloseTaskResponse{Task: toProtoTask(t)}, nil
}

func (s *server) CancelTask(ctx context.Context, req *workflowv1.CancelTaskRequest) (*workflowv1.CancelTaskResponse, error) {
	t, err := s.svc.CancelTask(ctx, repo.CancelTaskInput{
		IdempotencyKey: req.IdempotencyKey, TaskID: req.TaskId, Reason: req.Reason,
	})
	if err != nil {
		return nil, service.ToStatus(err)
	}
	return &workflowv1.CancelTaskResponse{Task: toProtoTask(t)}, nil
}

func (s *server) GetTaskStatus(ctx context.Context, req *workflowv1.GetTaskStatusRequest) (*workflowv1.TaskStatusResponse, error) {
	status, taskID, err := s.svc.GetTaskStatus(ctx, req.TaskId, req.IdempotencyKey)
	if err != nil {
		return nil, service.ToStatus(err)
	}
	return &workflowv1.TaskStatusResponse{Status: toProtoStatus(status), TaskId: taskID}, nil
}

func (s *server) BatchGetTasks(ctx context.Context, req *workflowv1.BatchGetTasksRequest) (*workflowv1.BatchGetTasksResponse, error) {
	tasks, err := s.svc.BatchGetTasks(ctx, req.TaskIds)
	if err != nil {
		return nil, service.ToStatus(err)
	}
	out := make([]*workflowv1.Task, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, toProtoTask(t))
	}
	return &workflowv1.BatchGetTasksResponse{Tasks: out}, nil
}

func (s *server) ListTasks(ctx context.Context, req *workflowv1.ListTasksRequest) (*workflowv1.ListTasksResponse, error) {
	tasks, nextCursor, err := s.svc.ListTasks(ctx, repo.ListInput{
		Type: fromProtoType(req.Type), Status: fromProtoStatus(req.Status),
		SourceComponent: req.SourceComponent, Cursor: req.Cursor, PageSize: req.PageSize,
	})
	if err != nil {
		return nil, service.ToStatus(err)
	}
	out := make([]*workflowv1.Task, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, toProtoTask(t))
	}
	return &workflowv1.ListTasksResponse{Tasks: out, NextCursor: nextCursor}, nil
}
