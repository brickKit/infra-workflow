// Package repo 是 infra-workflow 的数据访问层：待办主体、审批历史、
// 写命令幂等声明。三条铁律（设计计划 §6.6）在这一层的体现是"零表连
// 业务库"——本组件只存自己的三张表 + 标准 Outbox，没有一处 join 或
// 回查任何业务组件的 schema。
package repo

import (
	"database/sql"
	"errors"
	"fmt"
)

// ── 哨兵错误。grpc/http 两层通过 service.ToStatus 统一映射（同
// infra-authz/erp-finance 的既有判据）。────────────────────────────────

var ErrNotFound = errors.New("not found")
var ErrInvalidArgument = errors.New("参数不合法")

// ErrNotPending：对一个已经不是 PENDING 的待办调 approve/reject——
// 幂等意义上的"晚了一步"，不是系统错误（设计计划 §3 的 409 语义）。
var ErrNotPending = errors.New("待办已经不是 PENDING 状态")

// ErrForbidden：这条待办真实存在，调用者只是看不见/不能动它——同
// erp-sales/erp-inventory 的既有判据，与 ErrNotFound 语义不同，不能混用
// （service.checkTaskInScope 用它标记"越权"，不是"不存在"）。
var ErrForbidden = errors.New("无权访问该待办")

type Repo struct {
	db     *sql.DB
	role   string
	schema string
}

func New(db *sql.DB, role, schema string) *Repo {
	return &Repo{db: db, role: role, schema: schema}
}

func wrap(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", action, err)
}

func mapConstraintErr(err error, notFoundMsg string) error {
	switch {
	case isForeignKeyViolation(err):
		return fmt.Errorf("%w: %s", ErrNotFound, notFoundMsg)
	case isUniqueViolation(err):
		return fmt.Errorf("%w: 已存在", ErrInvalidArgument)
	default:
		return err
	}
}
