package service

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/brickKit/infra-workflow/v2/backend/internal/repo"
)

// 调用方自己能纠正的错误（参数不合法、幂等键被别的命令用过、找不到、待办已经
// 不是 PENDING）不是运维要处理的事：SDK 的访问日志与 RED 指标已经记下了状态码，
// 再记 ERROR 只会淹没真正要人处理的那几条。调用方断开或进程关停时的取消也不是
// 故障。ERROR 只留给映射成 Internal 的错误。

func bufferedService(t *testing.T, db *sql.DB) (*Service, *repo.Repo, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	r := repo.New(db, "infra_workflow_rw", "infra_workflow")
	return New(r, slog.New(slog.NewTextHandler(&logs, nil))), r, &logs
}

func validCreate(key string) repo.CreateTaskInput {
	return repo.CreateTaskInput{
		IdempotencyKey: key, Type: repo.TypeApproval,
		AssigneeSub: uniqueSuffix("assignee"), AssigneeDeptPath: "/1/12/",
		Title: "日志级别测试", SourceComponent: "erp/sales", SourceAggregate: "sales_order", SourceID: "log-1",
	}
}

func TestWrite_调用方错误与取消不记ERROR(t *testing.T) {
	svc, r, logs := bufferedService(t, testDB(t))
	ctx := context.Background()

	task, err := r.CreateTask(ctx, validCreate(uniqueSuffix("log-create")))
	if err != nil {
		t.Fatal(err)
	}
	id := taskIDOf(task)

	// 幂等键已经被 CreateTask 用过，再拿去 CloseTask → InvalidArgument（400）
	usedKey := uniqueSuffix("log-used")
	if _, err := svc.CreateTask(ctx, validCreate(usedKey)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CloseTask(ctx, repo.CloseTaskInput{IdempotencyKey: usedKey, TaskID: id}); err == nil {
		t.Fatal("被别的命令用过的幂等键应该报错")
	}
	// 不是数字的 task_id → InvalidArgument；不存在的 → NotFound（404）
	if _, err := svc.CancelTask(ctx, repo.CancelTaskInput{IdempotencyKey: uniqueSuffix("log-bad-id"), TaskID: "不是数字"}); err == nil {
		t.Fatal("不合法的 task_id 应该报错")
	}
	if _, err := svc.CloseTask(ctx, repo.CloseTaskInput{IdempotencyKey: uniqueSuffix("log-404"), TaskID: "999999999999"}); err == nil {
		t.Fatal("不存在的待办应该报错")
	}
	// 已经不是 PENDING → FailedPrecondition（REST 409）
	if _, err := svc.CloseTask(ctx, repo.CloseTaskInput{IdempotencyKey: uniqueSuffix("log-close"), TaskID: id}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CloseTask(ctx, repo.CloseTaskInput{IdempotencyKey: uniqueSuffix("log-close-again"), TaskID: id}); err == nil {
		t.Fatal("已关闭的待办再关闭应该报错")
	}
	if _, err := svc.CancelTask(ctx, repo.CancelTaskInput{IdempotencyKey: uniqueSuffix("log-cancel-closed"), TaskID: id}); err == nil {
		t.Fatal("已关闭的待办再作废应该报错")
	}
	// 调用方断开 / 关停 → 取消
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := svc.CreateTask(cancelled, validCreate(uniqueSuffix("log-cancelled"))); err == nil {
		t.Fatal("ctx 已取消时建待办应该报错")
	}

	if strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("调用方错误与取消不该记 ERROR，实际日志：\n%s", logs.String())
	}
}

// TestWrite_服务端故障仍记ERROR 是对照组：连接池已关闭是服务端故障（Internal），
// 运维要处理，必须照样是 ERROR。
func TestWrite_服务端故障仍记ERROR(t *testing.T) {
	db := testDB(t)
	svc, _, logs := bufferedService(t, db)
	db.Close()
	if _, err := svc.CreateTask(context.Background(), validCreate(uniqueSuffix("log-internal"))); err == nil {
		t.Fatal("连接池已关闭时应该报错")
	}
	if !strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("服务端故障应该记 ERROR，实际日志：\n%s", logs.String())
	}
}

func taskIDOf(t *repo.Task) string { return fmt.Sprint(t.ID) }
