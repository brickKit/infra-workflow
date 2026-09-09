// Package http 是 infra-workflow 的 REST 面（对外路径前缀
// /infra/workflow，与 assembly.yaml 的 edge_routes 一致）。⚠️
// CreateTask/CloseTask/CancelTask 永远不进 REST——它们只走 gRPC，是
// 组件间协议，不是人类操作（contracts/workflow.openapi.yaml 顶部的
// 三条铁律警告；同 erp-inventory TCC 三件套的既有判据）。
package http

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/brickKit/infra-workflow/backend/internal/repo"
	"github.com/brickKit/infra-workflow/backend/internal/service"
)

// RegisterRoutes 挂载业务路由——三个权限键均来自 assembly.yaml 的
// permissions 段，一字不差（漏写编译不过，见 besdk.GET/POST 的签名）。
func RegisterRoutes(eng *gin.Engine, svc *service.Service) {
	g := eng.Group("/infra/workflow")
	besdk.GET(g, "/tasks", "infra.workflow.task.view", listMyTasksHandler(svc))
	besdk.GET(g, "/tasks/:id", "infra.workflow.task.view", getTaskHandler(svc))
	besdk.POST(g, "/tasks/:id/approve", "infra.workflow.task.act", approveTaskHandler(svc))
	besdk.POST(g, "/tasks/:id/reject", "infra.workflow.task.act", rejectTaskHandler(svc))
	besdk.GET(g, "/admin/tasks", "infra.workflow.admin", listAdminTasksHandler(svc))
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

func listMyTasksHandler(svc *service.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		pageSize, _ := strconv.Atoi(c.Query("page_size"))
		tasks, nextCursor, err := svc.ListMyTasks(c.Request.Context(), repo.ListInput{
			Type: c.Query("type"), Status: c.Query("status"),
			Cursor: c.Query("cursor"), PageSize: int32(pageSize),
		})
		if err != nil {
			_ = c.Error(service.ToStatus(err))
			return
		}
		dtos := make([]gin.H, 0, len(tasks))
		for _, t := range tasks {
			dtos = append(dtos, toTaskDTO(t))
		}
		c.JSON(http.StatusOK, gin.H{"tasks": dtos, "next_cursor": nextCursor})
	}
}

func listAdminTasksHandler(svc *service.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		pageSize, _ := strconv.Atoi(c.Query("page_size"))
		tasks, nextCursor, err := svc.ListTasksAdmin(c.Request.Context(), repo.ListInput{
			Type: c.Query("type"), Status: c.Query("status"), AssigneeSub: c.Query("assignee_sub"),
			Cursor: c.Query("cursor"), PageSize: int32(pageSize),
		})
		if err != nil {
			_ = c.Error(service.ToStatus(err))
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
			_ = c.Error(service.ToStatus(err))
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
			_ = c.Error(service.ToStatus(err))
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
			_ = c.Error(service.ToStatus(err))
			return
		}
		c.JSON(http.StatusOK, toTaskDTO(t))
	}
}
