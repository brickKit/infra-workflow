package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	besdk "github.com/brickKit/be-sdk-go"
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

func testRepo(t *testing.T) *Repo {
	return New(testDB(t), "infra_workflow_rw", "infra_workflow")
}

// uniqueID 给每个测试造一个独立的 idempotency_key/sub/dept 前缀，测试之间
// 不共享行、互不干扰（同 infra-authz/infra-iam-casdoor repo_test.go 的
// 既有判据）。
var idSeq int64

func uniqueID(prefix string) string {
	n := atomic.AddInt64(&idSeq, 1)
	return fmt.Sprintf("%s_%d_%d", prefix, time.Now().UnixNano(), n)
}

func mustCreateTask(t *testing.T, r *Repo, in CreateTaskInput) *Task {
	t.Helper()
	task, err := r.CreateTask(context.Background(), in)
	if err != nil {
		t.Fatalf("建待办失败：%v", err)
	}
	return task
}

func TestCreateTask_基本创建(t *testing.T) {
	r := testRepo(t)
	assignee := uniqueID("sub")
	task := mustCreateTask(t, r, CreateTaskInput{
		IdempotencyKey: uniqueID("create"), Type: TypeApproval,
		AssigneeSub: assignee, AssigneeDeptPath: "/root/sales/",
		Title: "测试审批", Summary: json.RawMessage(`{"amount":"100.00"}`),
		SourceComponent: "erp/sales", SourceAggregate: "sales_order", SourceID: "order-1",
	})
	if task.Status != StatusPending {
		t.Fatalf("新建待办期望 PENDING，实际 %q", task.Status)
	}
	if task.AssigneeSub != assignee {
		t.Fatalf("assignee_sub 不匹配：%q", task.AssigneeSub)
	}
	// ⚠️ JSONB 会重新序列化（PostgreSQL 存 JSONB 时按自己的规范化格式
	// 落盘，比如冒号后面加空格），不是字节级原样——"透传"指的是语义层面
	// 的 JSON 值不变，不是字节数组不变，这里按解析后的值比较。
	var got, want map[string]any
	if err := json.Unmarshal(task.Summary, &got); err != nil {
		t.Fatalf("summary 不是合法 JSON：%v", err)
	}
	if err := json.Unmarshal([]byte(`{"amount":"100.00"}`), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("summary 应该原样透传，实际 %q", string(task.Summary))
	}

	var n int
	if err := besdk.WithTx(context.Background(), r.db, r.role, r.schema, func(tx *sql.Tx) error {
		return tx.QueryRow(
			`SELECT count(*) FROM event_outbox WHERE subject = 'infra.workflow.task.created.v1' AND aggregate_id = $1`,
			taskIDString(task.ID)).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("期望落 1 条 infra.workflow.task.created.v1，实际 %d 条", n)
	}
}

func TestCreateTask_幂等(t *testing.T) {
	r := testRepo(t)
	key := uniqueID("create-idem")
	in := CreateTaskInput{
		IdempotencyKey: key, Type: TypeApproval, AssigneeSub: uniqueID("sub"),
		Title: "幂等测试", SourceComponent: "erp/sales", SourceAggregate: "sales_order", SourceID: "order-2",
	}
	t1 := mustCreateTask(t, r, in)
	t2 := mustCreateTask(t, r, in)
	if t1.ID != t2.ID {
		t.Fatalf("幂等失效：第一次 id=%d，第二次 id=%d", t1.ID, t2.ID)
	}
}

func TestApproveTask_成功后再次操作返回ErrNotPending(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()
	task := mustCreateTask(t, r, CreateTaskInput{
		IdempotencyKey: uniqueID("approve"), Type: TypeApproval, AssigneeSub: uniqueID("sub"),
		Title: "待同意", SourceComponent: "erp/sales", SourceAggregate: "sales_order", SourceID: "order-3",
	})
	actorSub := task.AssigneeSub

	got, err := r.ApproveTask(ctx, taskIDString(task.ID), actorSub, "同意了")
	if err != nil {
		t.Fatalf("同意失败：%v", err)
	}
	if got.Status != StatusApproved {
		t.Fatalf("期望 APPROVED，实际 %q", got.Status)
	}

	// 再次操作（无论 approve 还是 reject）都必须报 ErrNotPending——已经
	// 不是 PENDING，claim-first 保护的是"同一个命令重放"，这里测的是
	// "状态确实不对时必须报错，不能悄悄吞掉"（actions.go 顶部注释）。
	// ⚠️ ApproveTask/RejectTask 用 wrap() 包了一层前缀（"同意待办: %w"），
	// 必须用 errors.Is 而不是直接比较，同 GetTaskStatus 那次
	// lookupIdempotencyResult 的既有教训。
	if _, err := r.ApproveTask(ctx, taskIDString(task.ID), actorSub, "再次同意"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("对已同意的待办再次同意应该报 ErrNotPending，实际 %v", err)
	}
	if _, err := r.RejectTask(ctx, taskIDString(task.ID), actorSub, "反悔"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("对已同意的待办驳回应该报 ErrNotPending，实际 %v", err)
	}

	var n int
	if err := besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		return tx.QueryRow(
			`SELECT count(*) FROM event_outbox WHERE subject = 'infra.workflow.task.completed.v1' AND aggregate_id = $1`,
			taskIDString(task.ID)).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("期望落 1 条 infra.workflow.task.completed.v1，实际 %d 条", n)
	}
}

func TestRejectTask_附言落库(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()
	task := mustCreateTask(t, r, CreateTaskInput{
		IdempotencyKey: uniqueID("reject"), Type: TypeApproval, AssigneeSub: uniqueID("sub"),
		Title: "待驳回", SourceComponent: "erp/sales", SourceAggregate: "sales_order", SourceID: "order-4",
	})
	got, err := r.RejectTask(ctx, taskIDString(task.ID), task.AssigneeSub, "金额不对")
	if err != nil {
		t.Fatalf("驳回失败：%v", err)
	}
	if got.Status != StatusRejected {
		t.Fatalf("期望 REJECTED，实际 %q", got.Status)
	}

	actions, err := r.ListTaskActions(ctx, taskIDString(task.ID))
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Action != ActionRejected || actions[0].Comment != "金额不对" {
		t.Fatalf("审批历史不符：%+v", actions)
	}
}

func TestCloseTask_幂等且发resolved事件(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()
	task := mustCreateTask(t, r, CreateTaskInput{
		IdempotencyKey: uniqueID("close-create"), Type: TypeException, AssigneeSub: uniqueID("sub"),
		Title: "待处理异常", SourceComponent: "erp/sales", SourceAggregate: "sales_order", SourceID: "order-5",
	})
	key := uniqueID("close")
	in := CloseTaskInput{IdempotencyKey: key, TaskID: taskIDString(task.ID), Comment: "已人工处理"}

	got1, err := r.CloseTask(ctx, in)
	if err != nil {
		t.Fatalf("关闭失败：%v", err)
	}
	if got1.Status != StatusResolved {
		t.Fatalf("期望 RESOLVED，实际 %q", got1.Status)
	}

	// 幂等重放：同一个 idempotency_key 再调一次必须成功且不报 ErrNotPending
	// （claim-first 应该在 transitionTaskTx 之前就短路掉，见 repo.go
	// wrap 那条设计判据）。
	got2, err := r.CloseTask(ctx, in)
	if err != nil {
		t.Fatalf("幂等重放不该报错：%v", err)
	}
	if got2.ID != got1.ID {
		t.Fatalf("幂等失效：第一次 id=%d，第二次 id=%d", got1.ID, got2.ID)
	}

	var n int
	if err := besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		return tx.QueryRow(
			`SELECT count(*) FROM event_outbox WHERE subject = 'infra.workflow.task.completed.v1' AND aggregate_id = $1`,
			taskIDString(task.ID)).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("幂等重放不该重复发事件，期望 1 条 completed，实际 %d 条", n)
	}
}

func TestCancelTask_发cancelled而非completed事件(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()
	task := mustCreateTask(t, r, CreateTaskInput{
		IdempotencyKey: uniqueID("cancel-create"), Type: TypeApproval, AssigneeSub: uniqueID("sub"),
		Title: "来源单据作废", SourceComponent: "erp/sales", SourceAggregate: "sales_order", SourceID: "order-6",
	})
	got, err := r.CancelTask(ctx, CancelTaskInput{
		IdempotencyKey: uniqueID("cancel"), TaskID: taskIDString(task.ID), Reason: "订单已取消",
	})
	if err != nil {
		t.Fatalf("作废失败：%v", err)
	}
	if got.Status != StatusCancelled {
		t.Fatalf("期望 CANCELLED，实际 %q", got.Status)
	}

	var completed, cancelled int
	if err := besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		if err := tx.QueryRow(
			`SELECT count(*) FROM event_outbox WHERE subject = 'infra.workflow.task.completed.v1' AND aggregate_id = $1`,
			taskIDString(task.ID)).Scan(&completed); err != nil {
			return err
		}
		return tx.QueryRow(
			`SELECT count(*) FROM event_outbox WHERE subject = 'infra.workflow.task.cancelled.v1' AND aggregate_id = $1`,
			taskIDString(task.ID)).Scan(&cancelled)
	}); err != nil {
		t.Fatal(err)
	}
	if completed != 0 {
		t.Fatalf("CancelTask 不该发 completed 事件，实际发了 %d 条", completed)
	}
	if cancelled != 1 {
		t.Fatalf("期望 1 条 cancelled 事件，实际 %d 条", cancelled)
	}
}

// TestGetTaskStatus_区分NotFound与Cancelled 是设计计划 §3.1 那条硬约束的
// 直接测试：合并成一个"没有"是错的（同 erp-inventory
// TestGetReservationStatus_区分NotFound与Cancelled 的既有判据）。
func TestGetTaskStatus_区分NotFound与Cancelled(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()

	status, _, err := r.GetTaskStatus(ctx, "999999999", "")
	if err != nil {
		t.Fatalf("查一个从没出现过的 task_id 不该报错：%v", err)
	}
	if status != "" {
		t.Fatalf("期望 NOT_FOUND（空字符串），实际 %q", status)
	}

	task := mustCreateTask(t, r, CreateTaskInput{
		IdempotencyKey: uniqueID("status-create"), Type: TypeApproval, AssigneeSub: uniqueID("sub"),
		Title: "待作废", SourceComponent: "erp/sales", SourceAggregate: "sales_order", SourceID: "order-7",
	})
	if _, err := r.CancelTask(ctx, CancelTaskInput{
		IdempotencyKey: uniqueID("status-cancel"), TaskID: taskIDString(task.ID), Reason: "测试",
	}); err != nil {
		t.Fatal(err)
	}

	status, resolvedID, err := r.GetTaskStatus(ctx, taskIDString(task.ID), "")
	if err != nil {
		t.Fatal(err)
	}
	if status != StatusCancelled {
		t.Fatalf("已作废的待办应该查到 CANCELLED，不是 NOT_FOUND，实际 %q", status)
	}
	if resolvedID != taskIDString(task.ID) {
		t.Fatalf("期望 task_id=%s，实际 %q", taskIDString(task.ID), resolvedID)
	}
}

// TestGetTaskStatus_按idempotencyKey查 验证 CreateTask 超时场景下唯一
// 能用的反查路径（设计计划 §3.1 ⭐，erp-inventory 阶段二真机故障注入
// 测试换来的教训——同一个判据搬到本组件）。
func TestGetTaskStatus_按idempotencyKey查(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()
	key := uniqueID("status-by-key")
	task := mustCreateTask(t, r, CreateTaskInput{
		IdempotencyKey: key, Type: TypeApproval, AssigneeSub: uniqueID("sub"),
		Title: "按key查", SourceComponent: "erp/sales", SourceAggregate: "sales_order", SourceID: "order-8",
	})

	status, resolvedID, err := r.GetTaskStatus(ctx, "", key)
	if err != nil {
		t.Fatal(err)
	}
	if status != StatusPending {
		t.Fatalf("期望 PENDING，实际 %q", status)
	}
	if resolvedID != taskIDString(task.ID) {
		t.Fatalf("按 idempotency_key 查到的 task_id 应该等于真正的 task_id，期望 %s 实际 %q",
			taskIDString(task.ID), resolvedID)
	}

	// 从没提交过的 idempotency_key 必须是真正的 NOT_FOUND，不是报错。
	status, _, err = r.GetTaskStatus(ctx, "", uniqueID("never-submitted"))
	if err != nil {
		t.Fatalf("查一个从没提交过的 idempotency_key 不该报错：%v", err)
	}
	if status != "" {
		t.Fatalf("期望 NOT_FOUND，实际 %q", status)
	}
}

func TestBatchGetTasks_查不到的id直接省略(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()
	task := mustCreateTask(t, r, CreateTaskInput{
		IdempotencyKey: uniqueID("batch"), Type: TypeApproval, AssigneeSub: uniqueID("sub"),
		Title: "批量查", SourceComponent: "erp/sales", SourceAggregate: "sales_order", SourceID: "order-9",
	})
	got, err := r.BatchGetTasks(ctx, []string{taskIDString(task.ID), "999999999", "不合法id"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != task.ID {
		t.Fatalf("期望只查到 1 条真实存在的待办，实际 %+v", got)
	}
}

// TestListTasks_我的待办用OR不是AND 是本次实现中修过的一个真实 bug 的
// 回归测试：assignee_sub 命中 ScopeOwner 或 assignee_dept_path 命中
// ScopePrefix 任一为真都该出现在"我的待办"列表里（workflow.openapi.yaml
// 明文），但 ScopePrefix 留空绝不能变成"看到所有部门"——那是另一个人
// 的部门时才是真正的越权。
func TestListTasks_我的待办用OR不是AND(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()
	me := uniqueID("sub")
	myDept := "/root/sales/east/" + uniqueID("dept") + "/"
	otherDept := "/root/finance/" + uniqueID("dept") + "/"
	source := uniqueID("src")

	// 分给我自己的，部门在别处（不该靠 org 维命中，只能靠 owner 维）。
	assignedToMe := mustCreateTask(t, r, CreateTaskInput{
		IdempotencyKey: uniqueID("list"), Type: TypeApproval, AssigneeSub: me, AssigneeDeptPath: otherDept,
		Title: "分给我", SourceComponent: source, SourceAggregate: "x", SourceID: "1",
	})
	// 分给我部门里另一个人的（不该靠 owner 维命中，只能靠 org 维）。
	inMyDept := mustCreateTask(t, r, CreateTaskInput{
		IdempotencyKey: uniqueID("list"), Type: TypeApproval, AssigneeSub: uniqueID("colleague"), AssigneeDeptPath: myDept,
		Title: "同部门", SourceComponent: source, SourceAggregate: "x", SourceID: "2",
	})
	// 既不是我、部门也不在我范围内——两个维度都不该命中。
	unrelated := mustCreateTask(t, r, CreateTaskInput{
		IdempotencyKey: uniqueID("list"), Type: TypeApproval, AssigneeSub: uniqueID("stranger"), AssigneeDeptPath: otherDept,
		Title: "无关", SourceComponent: source, SourceAggregate: "x", SourceID: "3",
	})

	tasks, _, err := r.ListTasks(ctx, ListInput{
		SourceComponent: source, ScopeOwner: me, ScopePrefix: myDept, PageSize: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[int64]bool{}
	for _, tk := range tasks {
		ids[tk.ID] = true
	}
	if !ids[assignedToMe.ID] {
		t.Fatal("分给我自己的待办应该出现在列表里（owner 维）")
	}
	if !ids[inMyDept.ID] {
		t.Fatal("我部门里的待办应该出现在列表里（org 维）")
	}
	if ids[unrelated.ID] {
		t.Fatal("无关的待办不该出现在列表里——ScopePrefix 留空不能等价于看到全部")
	}
}

// TestListTasks_AdminView绕过owner维但不绕过org维 对应 GET /admin/tasks
// 的契约明文（workflow.openapi.yaml）。
func TestListTasks_AdminView绕过owner维但不绕过org维(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()
	adminDept := "/root/hq/" + uniqueID("dept") + "/"
	outsideDept := "/root/other/" + uniqueID("dept") + "/"
	source := uniqueID("src-admin")

	inScope := mustCreateTask(t, r, CreateTaskInput{
		IdempotencyKey: uniqueID("admin-list"), Type: TypeApproval, AssigneeSub: uniqueID("sub"), AssigneeDeptPath: adminDept,
		Title: "admin 范围内", SourceComponent: source, SourceAggregate: "x", SourceID: "1",
	})
	outOfScope := mustCreateTask(t, r, CreateTaskInput{
		IdempotencyKey: uniqueID("admin-list"), Type: TypeApproval, AssigneeSub: uniqueID("sub"), AssigneeDeptPath: outsideDept,
		Title: "admin 范围外", SourceComponent: source, SourceAggregate: "x", SourceID: "2",
	})

	tasks, _, err := r.ListTasks(ctx, ListInput{
		SourceComponent: source, AdminView: true, ScopePrefix: adminDept, PageSize: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[int64]bool{}
	for _, tk := range tasks {
		ids[tk.ID] = true
	}
	if !ids[inScope.ID] {
		t.Fatal("admin 应该看到自己范围内、不是分配给自己的待办（绕过 owner 维）")
	}
	if ids[outOfScope.ID] {
		t.Fatal("admin 不该看到范围外部门的待办（不绕过 org 维）")
	}

	// AssigneeSub 精确过滤：admin 显式指定某个人，只应该命中那个人。
	narrowed, _, err := r.ListTasks(ctx, ListInput{
		SourceComponent: source, AdminView: true, ScopePrefix: adminDept, AssigneeSub: inScope.AssigneeSub, PageSize: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(narrowed) != 1 || narrowed[0].ID != inScope.ID {
		t.Fatalf("按 assignee_sub 精确过滤应该只返回那一条，实际 %+v", narrowed)
	}
}

func TestMarkOverdueAndPublish_只通知一次且不改status(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()
	task := mustCreateTask(t, r, CreateTaskInput{
		IdempotencyKey: uniqueID("overdue"), Type: TypeApproval, AssigneeSub: uniqueID("sub"),
		Title: "已超期", SourceComponent: "erp/sales", SourceAggregate: "sales_order", SourceID: "order-overdue",
	})
	past := time.Now().Add(-time.Hour)
	if err := besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE workflow_tasks SET due_at = $1 WHERE id = $2`, past, task.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	n, err := r.MarkOverdueAndPublish(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Fatalf("期望至少扫到 1 条超期待办，实际 %d", n)
	}

	got, err := r.GetTask(ctx, taskIDString(task.ID))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusPending {
		t.Fatalf("超期扫描绝不能修改 status，期望仍是 PENDING，实际 %q（§6.6 铁律一）", got.Status)
	}

	var eventCount int
	if err := besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		return tx.QueryRow(
			`SELECT count(*) FROM event_outbox WHERE subject = 'infra.workflow.task.overdue.v1' AND aggregate_id = $1`,
			taskIDString(task.ID)).Scan(&eventCount)
	}); err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 {
		t.Fatalf("期望 1 条 overdue 事件，实际 %d 条", eventCount)
	}

	// 再跑一轮：overdue_notified_at 已经非空，不该再次命中、不该再发一条。
	if _, err := r.MarkOverdueAndPublish(ctx); err != nil {
		t.Fatal(err)
	}
	if err := besdk.WithTx(ctx, r.db, r.role, r.schema, func(tx *sql.Tx) error {
		return tx.QueryRow(
			`SELECT count(*) FROM event_outbox WHERE subject = 'infra.workflow.task.overdue.v1' AND aggregate_id = $1`,
			taskIDString(task.ID)).Scan(&eventCount)
	}); err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 {
		t.Fatalf("超期通知只该发一次，第二轮扫描后期望仍是 1 条，实际 %d 条", eventCount)
	}
}
