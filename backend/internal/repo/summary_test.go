package repo

import (
	"context"
	"encoding/json"
	"testing"
)

// TestCreateTask_没给summary时存成空对象：summary_json 在契约里是可选的，gRPC
// 调用方不填时，到这里的是一个空字节串（不是 nil）。它必须落成 JSONB 的 {}，
// 而不是把空串塞进 JSONB 列——那会报 invalid input syntax for type json，
// 调用方拿到一个 Internal 错误，待办建不出来。
func TestCreateTask_没给summary时存成空对象(t *testing.T) {
	r := testRepo(t)
	task, err := r.CreateTask(context.Background(), CreateTaskInput{
		IdempotencyKey: uniqueID("no-summary"), Type: TypeException, AssigneeSub: uniqueID("sub"),
		Title: "没有展示快照", Summary: json.RawMessage(""),
		SourceComponent: "erp/sales", SourceAggregate: "sales_order", SourceID: "order-nosum",
	})
	if err != nil {
		t.Fatalf("summary 为空串时应该照常建待办，实际：%v", err)
	}
	if string(task.Summary) != "{}" {
		t.Fatalf("空 summary 应该存成 {}，实际 %q", task.Summary)
	}
}
