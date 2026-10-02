// Package repo 是 infra-workflow 的数据访问层：待办主体、审批历史、
// 写命令幂等声明。本组件不判业务规则、不查业务库：这一层只读写自己的三张表
// 与 Outbox，没有一处 join 或回查任何业务组件的 schema——待办要展示的业务
// 字段由调用方在 CreateTask 时作为快照传进来。
package repo

import (
	"database/sql"
	"errors"
	"fmt"
)

// ── 哨兵错误。grpc / http 两层通过 service.ToStatus 统一映射。──────────

var ErrNotFound = errors.New("not found")
var ErrInvalidArgument = errors.New("参数不合法")

// ErrNotPending：对一个已经不是 PENDING 的待办再做 approve / reject /
// close / cancel——"晚了一步"（别人或自己已经处理过），不是系统错误。
var ErrNotPending = errors.New("待办已经不是 PENDING 状态")

// ErrForbidden：这条待办真实存在，调用者只是看不见 / 不能动它。与
// ErrNotFound 不能混用：前端对 403 与 404 的提示不同。
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
