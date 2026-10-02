// Package module 是 infra-workflow 唯一的装配入口。独立运行（cmd/server 的
// besdk.RunStandalone）与进外壳走同一个 New：模块只交回零件（HTTP handler、
// gRPC 注册函数、后台循环），谁去 Listen、谁开连接池、谁初始化 OTel 与信号
// 处理，全归调用方——这样同一份代码进外壳之后不会与别的成员互相覆盖。
package module

import (
	"context"
	"log/slog"
	"time"

	besdk "github.com/brickKit/be-sdk-go"
	workflowv1 "github.com/brickKit/infra-workflow/gen/infra/workflow/v1"
	"google.golang.org/grpc"

	grpcapi "github.com/brickKit/infra-workflow/v2/backend/internal/grpc"
	httpapi "github.com/brickKit/infra-workflow/v2/backend/internal/http"
	"github.com/brickKit/infra-workflow/v2/backend/internal/partition"
	"github.com/brickKit/infra-workflow/v2/backend/internal/repo"
	"github.com/brickKit/infra-workflow/v2/backend/internal/service"
)

// New 构造 infra-workflow 模块。签名是外壳与 RunStandalone 共同依赖的约定，
// 不改。
func New(ctx context.Context, rt *besdk.Runtime) (*besdk.Module, error) {
	// 配置只从 rt.Config 读，模块里不碰 os.Getenv：一个进程只有一份环境，
	// 进外壳后各成员的 PG_SCHEMA 会互相覆盖，不报错，模块就按别人的 schema
	// 读写数据。OTEL_BASE_URL / IAM_JWKS_URL / AUTHZ_BUNDLE_URL 由 SDK 自己从
	// rt.Config 读（RunStandalone、NewGinEngine、RequirePermission），这里不读。
	schema := rt.Config.StringOr("PG_SCHEMA", "infra_workflow")
	// role 是每个事务里 SET LOCAL ROLE 的目标：独立运行时它就是登录角色；
	// 进外壳后外壳以自己的角色登录，靠这一步切到本组件的角色。
	role := schema + "_rw"

	// 连接池、日志、Tracer、Meter 都从 rt 来，不自己 sql.Open、不自己
	// SetTracerProvider：进程级的初始化"最后一个赢"。
	r := repo.New(rt.DB, role, schema)
	svc := service.New(r, rt.Logger)

	// engine 必须用 besdk.NewGinEngine：它挂好了 request-id、trace、RED 指标、
	// 访问日志、panic 恢复、错误到状态码的映射与 /healthz、/metrics。
	eng := besdk.NewGinEngine(rt)
	httpapi.RegisterRoutes(eng, svc)

	return &besdk.Module{
		HTTPHandler: eng,

		// gRPC 由调用方在 extraPorts 的 grpc 端口上 Listen。业务组件调本组件
		// 一律走 gRPC，进外壳后也不直接调函数：边界在合并时不消失，组件才能
		// 随时拆回独立部署。
		RegisterGRPC: func(gs *grpc.Server) {
			workflowv1.RegisterWorkflowServiceServer(gs, grpcapi.New(svc))
		},

		// 后台循环：Outbox 推送 + 超期扫描 + event_outbox 的周分区维护。三个
		// 循环必须并发跑：每个都阻塞到 ctx 取消才返回，顺序调用的话后面的
		// 永远轮不到。本组件不消费任何事件，所以没有 event_inbox、没有消费者
		// 循环；workflow_tasks 不分区，但 event_outbox 是分区表——迁移只建了
		// 最初几周的分区，没有维护循环，之后的 Outbox 写入会因为找不到分区
		// 失败，所有 task.* 事件都发不出去。
		Start: func(ctx context.Context) error {
			errCh := make(chan error, 3)
			go func() { errCh <- besdk.StartOutboxPump(ctx, rt.DB, schema, rt.NATS, rt.Logger) }()
			go func() { errCh <- startOverdueScan(ctx, svc, rt.Logger) }()
			go func() { errCh <- partition.Start(ctx, rt.DB, role, schema, rt.Logger) }()

			select {
			case <-ctx.Done():
				return nil
			case err := <-errCh:
				return err // 返回 error，不 log.Fatal：进外壳后一个成员退出进程，所有成员一起没了
			}
		},
		Stop: func(ctx context.Context) error { return nil }, // 后台循环靠 ctx 退出
	}, nil
}

// overdueScanInterval：超期只需要"及时"地报告，不需要秒级。漏报 overdue
// 的后果只是通知晚了，不像漏发 completed 那样让业务单据卡住；一分钟足够
// 及时，又不给数据库添负担。分区维护是另一种节奏（每天一次就够）。
const overdueScanInterval = time.Minute

// startOverdueScan 是 Module.Start 的后台循环之一：每 overdueScanInterval
// 检查一次 due_at 已过的 PENDING 待办并发 task.overdue.v1。一轮处理满
// repo.OverdueBatchSize 条说明可能还没扫完，立刻再扫一轮追上，而不是等下一个
// tick；单次失败只记日志、不让循环退出——循环退出就等于这个成员从此不再报超期，
// 直到进程重启。
func startOverdueScan(ctx context.Context, svc *service.Service, logger *slog.Logger) error {
	scanUntilCaughtUp := func() {
		for {
			n, err := svc.MarkOverdueAndPublish(ctx)
			if err != nil {
				logger.Error("扫描超期待办失败", "error", err)
				return
			}
			if n < repo.OverdueBatchSize {
				return
			}
		}
	}
	scanUntilCaughtUp()

	ticker := time.NewTicker(overdueScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			scanUntilCaughtUp()
		}
	}
}
