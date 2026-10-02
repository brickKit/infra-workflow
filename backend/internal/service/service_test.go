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
	"github.com/brickKit/infra-workflow/v2/backend/internal/repo"
	_ "github.com/jackc/pgx/v5/stdlib" // 锁定栈用 pgx 不用 lib/pq，驱动名注册为 "pgx"
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
// 才不会 panic。
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

// TestListMyTasks_service层注入两维ScopeOf：ListMyTasks 必须同时把 ScopeOwner
// 与 ScopePrefix 都从 ScopeOf(ctx) 填好。漏填 ScopePrefix 会让 SQL 里的
// 前缀匹配退化成 `LIKE '%'`（匹配全部），OR 之后"我的待办"看到所有人的待办。
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
	// 转办还没有做，所以严格要求被指派人本人。
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

// TestCreateTask_summary不是合法JSON返回参数错误：summary_json 是调用方给的
// 展示快照，写进 JSONB 列。不是合法 JSON 是调用方的错，应该是参数错误
// （gRPC InvalidArgument），而不是数据库报错之后变成 Internal。
func TestCreateTask_summary不是合法JSON返回参数错误(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.CreateTask(context.Background(), repo.CreateTaskInput{
		IdempotencyKey: uniqueSuffix("bad-summary"), Type: repo.TypeApproval, AssigneeSub: uniqueSuffix("sub"),
		Title: "坏快照", Summary: []byte(`{"amount": 100`),
		SourceComponent: "erp/sales", SourceAggregate: "sales_order", SourceID: "s-bad",
	})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("summary 不是合法 JSON 应该 ErrInvalidArgument，实际：%v", err)
	}
}

// ── 没分部门的人（dept_path 为空）：org 维必须落空，只剩本人。──────────
//
// authz 签发的真实部门路径总是 `/<id>/…/`（根部门也是 `/<根id>/`），所以
// dept_path 为空只有一种来源：这个人还没被分到任何部门（新账号的默认状态）。
// 把它当成"不限"，新开的、只被授了查看权限的账号就看得到全租户的待办。

func idsOf(tasks []*repo.Task) map[int64]bool {
	ids := map[int64]bool{}
	for _, tk := range tasks {
		ids[tk.ID] = true
	}
	return ids
}

func mustCreate(t *testing.T, r *repo.Repo, assignee, dept, source, sourceID string) *repo.Task {
	t.Helper()
	task, err := r.CreateTask(context.Background(), repo.CreateTaskInput{
		IdempotencyKey: uniqueSuffix("nodept"), Type: repo.TypeApproval,
		AssigneeSub: assignee, AssigneeDeptPath: dept,
		Title: "无部门测试", SourceComponent: source, SourceAggregate: "x", SourceID: sourceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestListMyTasks_无部门的人只看到指派给自己的待办(t *testing.T) {
	svc, r := newTestService(t)
	me := uniqueSuffix("nodept-me")
	source := uniqueSuffix("src-nodept")

	mine := mustCreate(t, r, me, "", source, "1")
	otherDept := mustCreate(t, r, uniqueSuffix("other"), "/9/99/", source, "2")
	otherNoDept := mustCreate(t, r, uniqueSuffix("other"), "", source, "3")

	tasks, _, err := svc.ListMyTasks(authedCtx(me, ""), repo.ListInput{SourceComponent: source, PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	ids := idsOf(tasks)
	if !ids[mine.ID] {
		t.Fatal("指派给我自己的待办应该出现（owner 维）")
	}
	if ids[otherDept.ID] {
		t.Fatal("别的部门里别人的待办不该出现：没分部门不是'不限'")
	}
	if ids[otherNoDept.ID] {
		t.Fatal("同样没分部门的别人的待办也不该出现：两个空路径不是'同一个部门'")
	}
	if len(tasks) != 1 {
		t.Fatalf("应该只有指派给我的那一条，实际 %d 条", len(tasks))
	}
}

func TestListTasksAdmin_无部门的管理员看不到任何部门的待办(t *testing.T) {
	svc, r := newTestService(t)
	source := uniqueSuffix("src-nodept-admin")

	mustCreate(t, r, uniqueSuffix("a"), "/9/99/", source, "1")
	mustCreate(t, r, uniqueSuffix("b"), "/1/", source, "2")
	mustCreate(t, r, uniqueSuffix("c"), "", source, "3")

	tasks, _, err := svc.ListTasksAdmin(authedCtx(uniqueSuffix("admin"), ""), repo.ListInput{SourceComponent: source, PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Fatalf("没分部门的管理员不该看到任何部门的待办，实际 %d 条", len(tasks))
	}
}

func TestGetTaskDetail_无部门的人看别人的待办是Forbidden(t *testing.T) {
	svc, r := newTestService(t)
	source := uniqueSuffix("src-nodept-detail")
	inDept := mustCreate(t, r, uniqueSuffix("assignee"), "/1/12/", source, "1")
	noDept := mustCreate(t, r, uniqueSuffix("assignee"), "", source, "2")

	for _, task := range []*repo.Task{inDept, noDept} {
		_, _, err := svc.GetTaskDetail(authedCtx(uniqueSuffix("nodept-viewer"), ""), fmt.Sprint(task.ID))
		if !errors.Is(err, repo.ErrForbidden) {
			t.Fatalf("没分部门的人看别人的待办（assignee_dept_path=%q）应该 ErrForbidden，实际：%v", task.AssigneeDeptPath, err)
		}
	}

	// 自己的待办照样看得到：没分部门只是 org 维落空，owner 维不受影响。
	me := uniqueSuffix("nodept-owner")
	mine := mustCreate(t, r, me, "", source, "3")
	if _, _, err := svc.GetTaskDetail(authedCtx(me, ""), fmt.Sprint(mine.ID)); err != nil {
		t.Fatalf("没分部门的人看自己的待办应该放行，实际：%v", err)
	}
}

// TestListTasks_gRPC系统视图AllDepts仍看全部：gRPC 的 ListTasks 是组件间调用，
// 没有用户身份，契约上就是"看全部"。修 dept 范围之后，这一支必须用显式的
// "全部部门"表达，不能被"空前缀不再是全部"误伤——包括 assignee_dept_path
// 为空的行。
func TestListTasks_gRPC系统视图AllDepts仍看全部(t *testing.T) {
	svc, r := newTestService(t)
	source := uniqueSuffix("src-system")
	a := mustCreate(t, r, uniqueSuffix("a"), "/9/99/", source, "1")
	b := mustCreate(t, r, uniqueSuffix("b"), "/1/12/", source, "2")
	c := mustCreate(t, r, uniqueSuffix("c"), "", source, "3")

	// 不带 Claims 的 ctx：与 gRPC 入口一致。
	tasks, _, err := svc.ListTasks(context.Background(), repo.ListInput{SourceComponent: source, PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	ids := idsOf(tasks)
	for _, want := range []*repo.Task{a, b, c} {
		if !ids[want.ID] {
			t.Fatalf("系统视图应该看到全部待办，缺了 assignee_dept_path=%q 的那条", want.AssigneeDeptPath)
		}
	}
}

// TestCreateTask_assignee_dept_path不是真实路径报参数错误：assignee_dept_path
// 是调用方给的快照，要么是真实路径（以 / 开头），要么留空（被指派人没分部门）。
// 调用方把自己的 ScopeFilter.Prefix 原样传进来时，没分部门的人会带来哨兵
// besdk.NoDeptPath；存进行里之后，所有没分部门的人的前缀都等于它，彼此看得到
// 对方的待办。所以不以 / 开头的非空值一律是调用方的错。
func TestCreateTask_assignee_dept_path不是真实路径报参数错误(t *testing.T) {
	svc, _ := newTestService(t)
	for _, dept := range []string{besdk.NoDeptPath, "1/12/", "abc"} {
		_, err := svc.CreateTask(context.Background(), repo.CreateTaskInput{
			IdempotencyKey: uniqueSuffix("bad-dept"), Type: repo.TypeApproval,
			AssigneeSub: uniqueSuffix("assignee"), AssigneeDeptPath: dept,
			Title: "坏路径", SourceComponent: "erp/sales", SourceAggregate: "sales_order", SourceID: "bd-1",
		})
		if !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("assignee_dept_path=%q 应该 ErrInvalidArgument，实际：%v", dept, err)
		}
	}
	for _, dept := range []string{"", "/", "/1/12/"} {
		if _, err := svc.CreateTask(context.Background(), repo.CreateTaskInput{
			IdempotencyKey: uniqueSuffix("ok-dept"), Type: repo.TypeApproval,
			AssigneeSub: uniqueSuffix("assignee"), AssigneeDeptPath: dept,
			Title: "好路径", SourceComponent: "erp/sales", SourceAggregate: "sales_order", SourceID: "bd-2",
		}); err != nil {
			t.Fatalf("assignee_dept_path=%q 应该照常建待办，实际：%v", dept, err)
		}
	}
}
