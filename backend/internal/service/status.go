package service

import (
	"errors"

	"github.com/brickKit/infra-workflow/backend/internal/repo"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ToStatus 把 repo 层的哨兵错误翻成 gRPC status——HTTP 与 gRPC 两条对外
// 接口共用同一套业务错误类型（同 erp-inventory/erp-sales/infra-authz 的
// 判据）。
//
// ⚠️ ErrNotPending → FailedPrecondition：对一个已经不是 PENDING 的待办
// 再次 approve/reject/close/cancel，是"状态不对"，不是"这个东西不存在"
// 也不是"参数写错了"（同 erp-inventory ErrInsufficientStock 的判据：
// RowsAffected 类冲突统一映射到 FailedPrecondition）。
func ToStatus(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, repo.ErrNotPending):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, repo.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, repo.ErrForbidden):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, ErrInvalidArgument), errors.Is(err, repo.ErrInvalidArgument):
		return status.Error(codes.InvalidArgument, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}
