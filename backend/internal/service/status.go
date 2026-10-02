package service

import (
	"errors"

	"github.com/brickKit/infra-workflow/v2/backend/internal/repo"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ToStatus 把 repo 层的哨兵错误翻成 gRPC status，HTTP 与 gRPC 两个面共用。
//
// ErrNotPending → FailedPrecondition：对一个已经不是 PENDING 的待办再次
// close / cancel，是"状态不满足前提"，不是"不存在"也不是"参数写错了"。
// REST 面对这一个错误另有翻译（409，见 http 包的 restStatus）。
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
