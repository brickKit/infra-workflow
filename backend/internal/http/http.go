// Package http 是 infra-workflow 的 REST 面（对外路径前缀
// /infra/workflow，与 assembly.yaml 的 edge_routes 一致）。⚠️
// CreateTask/CloseTask/CancelTask 永远不进 REST——它们只走 gRPC，是
// 组件间协议，不是人类操作（contracts/workflow.openapi.yaml 顶部的
// 三条铁律警告；同 erp-inventory TCC 三件套的既有判据）。
package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/brickKit/infra-workflow/v2/backend/internal/repo"
	"github.com/brickKit/infra-workflow/v2/backend/internal/service"
)

// RegisterRoutes 挂载业务路由——三个权限键均来自 assembly.yaml 的
// permissions 段，一字不差（漏写编译不过，见 besdk.GET/POST 的签名）。
func RegisterRoutes(eng *gin.Engine, svc *service.Service) {
	g := eng.Group("/infra/workflow")
	besdk.GET(g, "/tasks", "infra.workflow.task.view", listHandler(svc.ListMyTasks, false))
	besdk.GET(g, "/tasks/:id", "infra.workflow.task.view", getTaskHandler(svc))
	besdk.POST(g, "/tasks/:id/approve", "infra.workflow.task.act", approveTaskHandler(svc))
	besdk.POST(g, "/tasks/:id/reject", "infra.workflow.task.act", rejectTaskHandler(svc))
	besdk.GET(g, "/admin/tasks", "infra.workflow.admin", listHandler(svc.ListTasksAdmin, true))
}

// restStatus 是 REST 面的错误翻译：与 gRPC 共用 service.ToStatus，只有
// "待办已经不是 PENDING"一处不同。gRPC 里它是 FailedPrecondition（状态不满足
// 前提，调用方是业务组件）；但 SDK 把 FailedPrecondition 映射成 400，而 REST
// 契约对它写的是 409——前端靠 409 区分"已经被处理了，刷新一下"与"请求写错了"。
// Aborted 在 SDK 的映射里正是 409。
func restStatus(err error) error {
	if errors.Is(err, repo.ErrNotPending) {
		return status.Error(codes.Aborted, err.Error())
	}
	return service.ToStatus(err)
}

const rfc3339 = "2006-01-02T15:04:05.999999999Z07:00"

// toTaskDTO 的 id 字段必须序列化成字符串——contracts/workflow.openapi.yaml
// 的 Task.id 是 string（同 CreateTaskRequest 等 proto 消息里 task_id 一律
// string 的既有约定），t.ID 在 repo 层是 BIGINT，这里转一次。
func toTaskDTO(t *repo.Task) gin.H {
	dto := gin.H{
		"id": strconv.FormatInt(t.ID, 10), "type": t.Type, "status": t.Status,
		"assignee_sub": t.AssigneeSub, "assignee_dept_path": t.AssigneeDeptPath,
		"title": t.Title, "summary": jsonRawOrNull(t.Summary),
		"source_component": t.SourceComponent, "source_aggregate": t.SourceAggregate, "source_id": t.SourceID,
		"deep_link":  t.DeepLink,
		"created_at": t.CreatedAt.Format(rfc3339), "updated_at": t.UpdatedAt.Format(rfc3339),
	}
	if t.DueAt != nil {
		dto["due_at"] = t.DueAt.Format(rfc3339)
	} else {
		dto["due_at"] = nil
	}
	return dto
}

// jsonRawOrNull 把 Summary（原始 JSON 字节，本组件不反解，见 repo.Task
// 注释）原样嵌进响应体——用 json.RawMessage 让 gin 的 JSON 编码器直接
// 透传字节，不重新序列化一遍。
func jsonRawOrNull(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	return json.RawMessage(raw)
}

func toTaskActionDTO(a repo.TaskAction) gin.H {
	return gin.H{
		"actor_sub": a.ActorSub, "action": a.Action, "comment": a.Comment,
		"created_at": a.CreatedAt.Format(rfc3339),
	}
}

// listFunc 是两个列表端点背后的 service 方法（ListMyTasks / ListTasksAdmin）：
// 两者的查询参数与响应形状相同，只有数据范围的判法不同，那一半在 service 层。
type listFunc func(ctx context.Context, in repo.ListInput) ([]*repo.Task, string, error)

// listHandler 读列表的查询参数；assignee_sub 只有管理视图认（"我的待办"的
// 归属恒等于调用者自己，不接受调用方指定）。
func listHandler(list listFunc, acceptAssignee bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		pageSize, _ := strconv.Atoi(c.Query("page_size"))
		in := repo.ListInput{
			Type: c.Query("type"), Status: c.Query("status"),
			Cursor: c.Query("cursor"), PageSize: int32(pageSize),
		}
		if acceptAssignee {
			in.AssigneeSub = c.Query("assignee_sub")
		}
		tasks, nextCursor, err := list(c.Request.Context(), in)
		if err != nil {
			_ = c.Error(restStatus(err))
			return
		}
		dtos := make([]gin.H, 0, len(tasks))
		for _, t := range tasks {
			dtos = append(dtos, toTaskDTO(t))
		}
		c.JSON(http.StatusOK, gin.H{"tasks": dtos, "next_cursor": nextCursor})
	}
}

func getTaskHandler(svc *service.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		t, actions, err := svc.GetTaskDetail(c.Request.Context(), c.Param("id"))
		if err != nil {
			_ = c.Error(restStatus(err))
			return
		}
		dto := toTaskDTO(t)
		actionDTOs := make([]gin.H, 0, len(actions))
		for _, a := range actions {
			actionDTOs = append(actionDTOs, toTaskActionDTO(a))
		}
		dto["actions"] = actionDTOs
		c.JSON(http.StatusOK, dto)
	}
}

type actionRequest struct {
	Comment string `json:"comment"`
}

func approveTaskHandler(svc *service.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req actionRequest
		if c.Request.ContentLength != 0 {
			if err := c.ShouldBindJSON(&req); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
		}
		t, err := svc.ApproveTask(c.Request.Context(), c.Param("id"), req.Comment)
		if err != nil {
			_ = c.Error(restStatus(err))
			return
		}
		c.JSON(http.StatusOK, toTaskDTO(t))
	}
}

// rejectTaskHandler 的附言用 binding:"required" 强制非空——http 层的
// 请求体绑定校验（同设计计划 §3 的 REST 表：驳回必须带附言，与
// service.RejectTask 的兜底校验是两道防线，不是重复劳动）。
type rejectRequest struct {
	Comment string `json:"comment" binding:"required"`
}

func rejectTaskHandler(svc *service.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req rejectRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		t, err := svc.RejectTask(c.Request.Context(), c.Param("id"), req.Comment)
		if err != nil {
			_ = c.Error(restStatus(err))
			return
		}
		c.JSON(http.StatusOK, toTaskDTO(t))
	}
}
