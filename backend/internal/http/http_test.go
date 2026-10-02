package http

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/trace/noop"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/brickKit/infra-workflow/v2/backend/internal/repo"
	"github.com/brickKit/infra-workflow/v2/backend/internal/service"
)

// 这里测的是 REST 层的形状：响应体的字段、错误到 HTTP 状态码的映射。engine
// 用 SDK 的 NewGinEngine（错误映射在它的中间件里），handler 挂在一个先往
// ctx 里放好 Claims 的路由组上——验签与权限键判定是 SDK 的事，由 SDK 自己
// 的测试守，这里不经过 RequirePermission。

func testService(t *testing.T) (*service.Service, *repo.Repo) {
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
	r := repo.New(db, "infra_workflow_rw", "infra_workflow")
	return service.New(r, slog.Default()), r
}

var seq int64

func unique(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), atomic.AddInt64(&seq, 1))
}

// engineAs 返回一个以 sub / deptPath 身份发请求的 engine，路由与
// RegisterRoutes 相同，只是不经过权限中间件。
func engineAs(svc *service.Service, sub, deptPath string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	rt := &besdk.Runtime{
		Logger:   slog.Default(),
		Tracer:   noop.NewTracerProvider().Tracer("test"),
		Registry: prometheus.NewRegistry(),
	}
	eng := besdk.NewGinEngine(rt)
	g := eng.Group("/infra/workflow", func(c *gin.Context) {
		ctx := besdk.ContextWithClaims(c.Request.Context(), besdk.Claims{Sub: sub, DeptPath: deptPath})
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	g.GET("/tasks/:id", getTaskHandler(svc))
	g.POST("/tasks/:id/approve", approveTaskHandler(svc))
	g.POST("/tasks/:id/reject", rejectTaskHandler(svc))
	return eng
}

func do(t *testing.T, eng *gin.Engine, method, path, body string) (int, map[string]any) {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	eng.ServeHTTP(w, req)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s 的响应不是 JSON 对象：%v（%s）", method, path, err, w.Body.String())
	}
	return w.Code, out
}

func createTask(t *testing.T, r *repo.Repo, assignee, dept string) string {
	t.Helper()
	task, err := r.CreateTask(context.Background(), repo.CreateTaskInput{
		IdempotencyKey: unique("http"), Type: repo.TypeApproval,
		AssigneeSub: assignee, AssigneeDeptPath: dept,
		Title: "REST 形状测试", SourceComponent: "erp/sales", SourceAggregate: "sales_order", SourceID: unique("so"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprint(task.ID)
}

func actionsOf(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["actions"]
	if !ok {
		t.Fatalf("响应里没有 actions 字段（契约 TaskDetail 带审批历史）：%v", body)
	}
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("actions 应该是数组（没有历史时是空数组，不是 null），实际 %T：%v", raw, raw)
	}
	out := make([]map[string]any, 0, len(list))
	for _, a := range list {
		out = append(out, a.(map[string]any))
	}
	return out
}

// TestGetTask_返回TaskDetail含审批历史：GET /tasks/{id} 是待办详情页与移动端
// task(id) 的数据来源，契约是 TaskDetail = Task 的字段 + actions（审批历史）。
// 还没有人处理时 actions 是空数组；同意之后多一条，带处理人、动作、附言、时间。
func TestGetTask_返回TaskDetail含审批历史(t *testing.T) {
	svc, r := testService(t)
	assignee := unique("assignee")
	id := createTask(t, r, assignee, "/1/12/")
	eng := engineAs(svc, assignee, "/1/12/")

	code, body := do(t, eng, http.MethodGet, "/infra/workflow/tasks/"+id, "")
	if code != http.StatusOK {
		t.Fatalf("GET /tasks/%s 期望 200，实际 %d：%v", id, code, body)
	}
	for _, k := range []string{"id", "type", "status", "assignee_sub", "assignee_dept_path", "title", "summary",
		"source_component", "source_aggregate", "source_id", "deep_link", "due_at", "created_at", "updated_at"} {
		if _, ok := body[k]; !ok {
			t.Errorf("响应缺少 Task 字段 %q", k)
		}
	}
	if body["id"] != id || body["status"] != repo.StatusPending {
		t.Fatalf("id / status 不对：%v / %v", body["id"], body["status"])
	}
	if got := actionsOf(t, body); len(got) != 0 {
		t.Fatalf("还没有人处理，actions 应该为空，实际 %v", got)
	}

	if _, err := svc.ApproveTask(besdk.ContextWithClaims(context.Background(), besdk.Claims{Sub: assignee, DeptPath: "/1/12/"}), id, "同意，按合同价"); err != nil {
		t.Fatal(err)
	}
	code, body = do(t, eng, http.MethodGet, "/infra/workflow/tasks/"+id, "")
	if code != http.StatusOK {
		t.Fatalf("同意之后 GET 期望 200，实际 %d：%v", code, body)
	}
	got := actionsOf(t, body)
	if len(got) != 1 {
		t.Fatalf("同意之后应该有 1 条审批历史，实际 %d 条：%v", len(got), got)
	}
	a := got[0]
	if a["actor_sub"] != assignee || a["action"] != repo.ActionApproved || a["comment"] != "同意，按合同价" {
		t.Fatalf("审批历史内容不对：%v", a)
	}
	if s, _ := a["created_at"].(string); s == "" {
		t.Fatalf("审批历史缺 created_at：%v", a)
	}
}

// TestGetTask_范围外403：既不是被指派人、部门也不在调用者范围内，详情返回
// 403（待办真实存在，不是 404）。
func TestGetTask_范围外403(t *testing.T) {
	svc, r := testService(t)
	id := createTask(t, r, unique("assignee"), "/1/12/")
	code, body := do(t, engineAs(svc, unique("stranger"), "/9/99/"), http.MethodGet, "/infra/workflow/tasks/"+id, "")
	if code != http.StatusForbidden {
		t.Fatalf("范围外期望 403，实际 %d：%v", code, body)
	}
}
