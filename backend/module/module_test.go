package module

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib" // 锁定栈用 pgx 不用 lib/pq，驱动名注册为 "pgx"

	"github.com/brickKit/infra-workflow/v2/backend/internal/repo"
	"github.com/brickKit/infra-workflow/v2/backend/internal/service"
)

// TestStartOverdueScan_关停时的取消不记ERROR：进程关停取消 ctx 时，正在跑的超期
// 扫描会以 context canceled 失败——这是关停，不是故障，不该记 ERROR。
func TestStartOverdueScan_关停时的取消不记ERROR(t *testing.T) {
	dsn := os.Getenv("TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 TEST_PG_DSN")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	svc := service.New(repo.New(db, "infra_workflow_rw", "infra_workflow"), logger)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := startOverdueScan(ctx, svc, logger); err != nil {
		t.Fatalf("ctx 取消时超期扫描应该正常返回，实际 %v", err)
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("关停时的取消不该记 ERROR，实际日志：\n%s", logs.String())
	}
}
