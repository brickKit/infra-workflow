// Package module 是 infra-workflow 唯一的装配入口（全局约束 §K、设计书
// §12.5.1、§13.3 铁律七）。单跑与合并走同一个 New 函数；模块只交回零件
// （handler、gRPC 注册函数、迁移、后台循环），谁去 Listen、谁开池、
// 谁 init OTel、谁装信号处理器，全归调用方。
package module

import (
	"context"
	"log/slog"
	"time"

	besdk "github.com/brickKit/be-sdk-go"
	workflowv1 "github.com/brickKit/infra-workflow/gen/infra/workflow/v1"
	"google.golang.org/grpc"

	grpcapi "github.com/brickKit/infra-workflow/backend/internal/grpc"
	httpapi "github.com/brickKit/infra-workflow/backend/internal/http"
	"github.com/brickKit/infra-workflow/backend/internal/repo"
	"github.com/brickKit/infra-workflow/backend/internal/service"
	"github.com/brickKit/infra-workflow/migrations"
)

// New 构造 infra-workflow 模块。签名一个字都不许改（§12.5.1）——62 个
// 组件都是这一个签名，外壳启动器与 be-ops 产出 4 都按它生成。
func New(ctx context.Context, rt *besdk.Runtime) (*besdk.Module, error) {
	// ⚠️ 配置只从 rt.Config 来，模块里零 os.Getenv（§12.5.3、决策 110）。
	// otelBaseUrl/iamJwksUrl/authzBundleUrl 不需要在这里显式读——
	// besdk.NewGinEngine/RequirePermission 内部自己从 rt.Config 取（同
	// erp-inventory 的既有判据：它也声明了这两项，module.go 里同样没有
	// 出现）。
	schema := rt.Config.StringOr("pgSchema", "infra_workflow")
	role := schema + "_rw"

	// ⚠️ 池从 rt.DB 来，不许自己 sql.Open（§13.3 铁律二）。
	r := repo.New(rt.DB, role, schema)
	svc := service.New(r, rt.Logger)

	// HTTP：engine 必须用 besdk.NewGinEngine，它已挂好 OTel / request-id /
	// error→status / PII 脱敏日志 / RED 指标 / /healthz / /metrics。
	eng := besdk.NewGinEngine(rt)
	httpapi.RegisterRoutes(eng, svc)

	return &besdk.Module{
		HTTPHandler: eng,

		// ⚠️ gRPC 一个不省，而且由调用方在 extraPorts["grpc"] 上 Listen
		// （§1.5 原则一）。
		RegisterGRPC: func(gs *grpc.Server) {
			workflowv1.RegisterWorkflowServiceServer(gs, grpcapi.New(svc))
		},

		Migrations: migrations.FS, // 合并态由外壳按拓扑顺序跑（§13.3 铁律五）

		// 后台循环：Outbox 推送 + 超期扫描。两个循环必须并发跑，不能顺序
		// 调用——它们各自是阻塞到 ctx 取消才返回的循环。⚠️ 本组件没有
		// event_inbox（零消费，§6.6 铁律三），不需要消费者循环；也不需要
		// 分区维护（workflow_tasks 不分区，见设计计划 §9 第 2 条）。
		Start: func(ctx context.Context) error {
			errCh := make(chan error, 2)
			go func() { errCh <- besdk.StartOutboxPump(ctx, rt.DB, schema, rt.NATS, rt.Logger) }()
			go func() { errCh <- startOverdueScan(ctx, svc, rt.Logger) }()

			select {
			case <-ctx.Done():
				return nil
			case err := <-errCh:
				return err // ⚠️ 返回 error，不许 log.Fatal：一个模块退进程 = 整组组件一起没了
			}
		},
		Stop: func(ctx context.Context) error { return nil }, // 后台循环靠 ctx 退出
	}, nil
}

// overdueScanInterval 与 partition.Start 的 24 小时检查周期不是同一类
// 节奏——那是"要不要新建分区"，几乎不紧急；这是"待办超期了没人知道"，
// 需要相对及时地报告（设计计划 §2、§4：漏发 overdue 事件的后果只是
// "通知晚了"，不像漏发 completed 那样会让业务单据卡死，所以不需要秒级
// 轮询，一分钟是足够及时又不给数据库添负担的折衷）。
const overdueScanInterval = time.Minute

// startOverdueScan 是 Module.Start 的后台循环之一：每 overdueScanInterval
// 检查一次 due_at 已过的 PENDING 待办并发 task.overdue.v1（设计计划
// §2、§4）。⚠️ MarkOverdueAndPublish 一批最多处理 100 条（repo/overdue.go
// 注释）——本轮处理满 100 条说明可能还没扫完，立刻再跑一轮追上而不是
// 等下一个 tick；单次失败只记日志，不让循环退出（同 partition.Start 的
// 既有判据，§13.3 铁律七）。
func startOverdueScan(ctx context.Context, svc *service.Service, logger *slog.Logger) error {
	scanUntilCaughtUp := func() {
		for {
			n, err := svc.MarkOverdueAndPublish(ctx)
			if err != nil {
				logger.Error("扫描超期待办失败", "error", err)
				return
			}
			if n < 100 {
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
