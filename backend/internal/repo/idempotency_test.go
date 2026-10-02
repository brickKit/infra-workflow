package repo

import (
	"context"
	"errors"
	"testing"
)

// TestIdempotencyKey_被另一个命令用过时报错而不是返回别人的结果：
// idempotency_key 只对"同一个命令的重放"短路。调用方拿一个已经被 CreateTask
// 用过的 key 去调 CloseTask / CancelTask（拼 key 时撞了），如果照样短路，
// 就会把 CreateTask 的结果当成"已经关闭 / 作废过"返回：待办其实还是
// PENDING，业务组件却以为关掉了，之后再也不会有人处理它。必须报参数错误，
// 待办保持原状。
func TestIdempotencyKey_被另一个命令用过时报错而不是返回别人的结果(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()
	key := uniqueID("shared-key")
	task := mustCreateTask(t, r, CreateTaskInput{
		IdempotencyKey: key, Type: TypeException, AssigneeSub: uniqueID("sub"),
		Title: "撞 key", SourceComponent: "erp/sales", SourceAggregate: "sales_order", SourceID: "order-key",
	})
	id := taskIDString(task.ID)

	if _, err := r.CloseTask(ctx, CloseTaskInput{IdempotencyKey: key, TaskID: id, Comment: "已处理"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("CloseTask 用了 CreateTask 的 key，期望 ErrInvalidArgument，实际 %v", err)
	}
	if _, err := r.CancelTask(ctx, CancelTaskInput{IdempotencyKey: key, TaskID: id, Reason: "单据作废"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("CancelTask 用了 CreateTask 的 key，期望 ErrInvalidArgument，实际 %v", err)
	}
	got, err := r.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusPending {
		t.Fatalf("两次错误调用之后待办应该仍是 PENDING，实际 %q", got.Status)
	}

	// 反方向：CloseTask 先用了一个 key，CreateTask 再拿它来建待办，同样报错，
	// 不能把被关闭的那条待办当成"新建成功"返回。
	closeKey := uniqueID("close-first")
	if _, err := r.CloseTask(ctx, CloseTaskInput{IdempotencyKey: closeKey, TaskID: id, Comment: "已处理"}); err != nil {
		t.Fatal(err)
	}
	_, err = r.CreateTask(ctx, CreateTaskInput{
		IdempotencyKey: closeKey, Type: TypeApproval, AssigneeSub: uniqueID("sub"),
		Title: "撞 key 新建", SourceComponent: "erp/sales", SourceAggregate: "sales_order", SourceID: "order-key-2",
	})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("CreateTask 用了 CloseTask 的 key，期望 ErrInvalidArgument，实际 %v", err)
	}
}
