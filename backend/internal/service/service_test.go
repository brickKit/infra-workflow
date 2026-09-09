package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/brickKit/infra-workflow/backend/internal/repo"
	_ "github.com/jackc/pgx/v5/stdlib" // §12.4：不用 lib/pq，驱动名注册为 "pgx"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 TEST_PG_DSN")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newTestService(t *testing.T) (*Service, *repo.Repo) {
	t.Helper()
	r := repo.New(testDB(t), "infra_workflow_rw", "infra_workflow")
	return New(r, slog.Default()), r
}

var svcSeq int64

func uniqueSuffix(prefix string) string {
	n := atomic.AddInt64(&svcSeq, 1)
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), n)
}

// authedCtx 造一个"已经过 RequirePermission 验签"的 ctx，besdk.ScopeOf
// 才不会 panic（同 erp-sales service_test.go 的既有判据）。
func authedCtx(sub, deptPath string) context.Context {
	return besdk.ContextWithClaims(context.Background(), besdk.Claims{Sub: sub, DeptPath: deptPath})
}

func TestGetTaskDetail_范围内OR命中(t *testing.T) {
	svc, r := newTestService(t)
	assignee := uniqueSuffix("assignee")
	dept := "/1/12/"
	task, err := r.CreateTask(context.Background(), repo.CreateTaskInput{
		IdempotencyKey: uniqueSuffix("detail"), Type: repo.TypeApproval,
		AssigneeSub: assignee, AssigneeDeptPath: dept,
		Title: "详情测试", SourceComponent: "erp/sales", SourceAggregate: "sales_order", SourceID: "d-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprint(task.ID)

	// owner 维精确命中：即使部门完全不搭边。
	got, _, err := svc.GetTaskDetail(authedCtx(assignee, "/9/99/"), id)
	if err != nil {
		t.Fatalf("owner 维命中应该放行，实际：%v", err)
	}
	if got.ID != task.ID {
		t.Fatal("查到的待办 id 不对")
	}

	// org 维前缀命中：即使不是 assignee 本人。
	got, _, err = svc.GetTaskDetail(authedCtx(uniqueSuffix("manager"), "/1/12/"), id)
	if err != nil {
		t.Fatalf("org 维命中应该放行，实际：%v", err)
	}
	if got.ID != task.ID {
		t.Fatal("查到的待办 id 不对")
	}
}

func TestGetTaskDetail_范围外ErrForbidden(t *testing.T) {
	svc, r := newTestService(t)
	task, err := r.CreateTask(context.Background(), repo.CreateTaskInput{
		IdempotencyKey: uniqueSuffix("detail-out"), Type: repo.TypeApproval,
		AssigneeSub: uniqueSuffix("assignee"), AssigneeDeptPath: "/1/12/",
		Title: "范围外测试", SourceComponent: "erp/sales", SourceAggregate: "sales_order", SourceID: "d-2",
	})
	if err != nil {
		t.Fatal(err)
	}

	// 既不是 assignee，部门也不搭边——必须是 ErrForbidden，不是悄悄放行，
	// 也不是 ErrNotFound（待办真实存在）。
	_, _, err = svc.GetTaskDetail(authedCtx(uniqueSuffix("stranger"), "/9/99/"), fmt.Sprint(task.ID))
	if !errors.Is(err, repo.ErrForbidden) {
		t.Fatalf("范围外应该是 ErrForbidden，实际：%v", err)
	}
}

// TestListMyTasks_service层注入两维ScopeOf 是本次实现修过的真实 bug 的
// 服务层回归：ListMyTasks 必须同时把 ScopeOwner 与 ScopePrefix 都从
// ScopeOf(ctx) 填好，遗漏 ScopePrefix 会让 SQL 里的 `LIKE ” || '%'`
// 退化成匹配全部，等于让"我的待办"看到所有人的待办。
func TestListMyTasks_service层注入两维ScopeOf(t *testing.T) {
	svc, r := newTestService(t)
	ctx := context.Background()
	me := uniqueSuffix("sub")
	myDept := "/1/12/"
	stranger := uniqueSuffix("stranger")
	source := uniqueSuffix("src")

	mine, err := r.CreateTask(ctx, repo.CreateTaskInput{
		IdempotencyKey: uniqueSuffix("mylist"), Type: repo.TypeApproval,
		AssigneeSub: me, AssigneeDeptPath: "/9/99/",
		Title: "我的", SourceComponent: source, SourceAggregate: "x", SourceID: "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	unrelated, err := r.CreateTask(ctx, repo.CreateTaskInput{
		IdempotencyKey: uniqueSuffix("mylist"), Type: repo.TypeApproval,
		AssigneeSub: stranger, AssigneeDeptPath: "/9/99/",
		Title: "无关", SourceComponent: source, SourceAggregate: "x", SourceID: "2",
	})
	if err != nil {
		t.Fatal(err)
	}

	tasks, _, err := svc.ListMyTasks(authedCtx(me, myDept), repo.ListInput{SourceComponent: source, PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[int64]bool{}
	for _, tk := range tasks {
		ids[tk.ID] = true
	}
	if !ids[mine.ID] {
		t.Fatal("分给我自己的待办应该出现在'我的待办'里")
	}
	if ids[unrelated.ID] {
		t.Fatal("与我无关的待办不该出现——ScopePrefix 必须真的从 ScopeOf(ctx) 取值，不能留空")
	}
}

func TestActorInScope_只有本人能approve不接受部门主管代批(t *testing.T) {
	svc, r := newTestService(t)
	ctx := context.Background()
	assignee := uniqueSuffix("assignee")
	task, err := r.CreateTask(ctx, repo.CreateTaskInput{
		IdempotencyKey: uniqueSuffix("actor"), Type: repo.TypeApproval,
		AssigneeSub: assignee, AssigneeDeptPath: "/1/12/",
		Title: "只能本人批", SourceComponent: "erp/sales", SourceAggregate: "sales_order", SourceID: "a-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprint(task.ID)

	// 部门主管即使 org 维能"看见"（GetTaskDetail 会放行），也不能代批——
	// "转办"是设计计划 §9 明确列出的未做功能，本阶段严格要求本人。
	_, err = svc.ApproveTask(authedCtx(uniqueSuffix("manager"), "/1/12/"), id, "我帮你批了")
	if !errors.Is(err, repo.ErrForbidden) {
		t.Fatalf("非本人 approve 应该 ErrForbidden，实际：%v", err)
	}

	got, err := svc.ApproveTask(authedCtx(assignee, "/1/12/"), id, "同意")
	if err != nil {
		t.Fatalf("本人 approve 应该成功，实际：%v", err)
	}
	if got.Status != repo.StatusApproved {
		t.Fatalf("期望 APPROVED，实际 %q", got.Status)
	}
}

func TestRejectTask_缺附言ErrInvalidArgument(t *testing.T) {
	svc, r := newTestService(t)
	ctx := context.Background()
	assignee := uniqueSuffix("assignee")
	task, err := r.CreateTask(ctx, repo.CreateTaskInput{
		IdempotencyKey: uniqueSuffix("reject-empty"), Type: repo.TypeApproval,
		AssigneeSub: assignee, AssigneeDeptPath: "/1/12/",
		Title: "驳回必须带附言", SourceComponent: "erp/sales", SourceAggregate: "sales_order", SourceID: "r-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.RejectTask(authedCtx(assignee, "/1/12/"), fmt.Sprint(task.ID), "")
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("空附言应该 ErrInvalidArgument，实际：%v", err)
	}
}
