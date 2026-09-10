package partition

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestMondayOf(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"2026-09-06", "2026-08-31"}, // Sunday -> 上一周一
		{"2026-08-31", "2026-08-31"}, // Monday -> 自己
		{"2026-09-02", "2026-08-31"}, // Wednesday -> 本周一
	}
	for _, c := range cases {
		in, err := time.Parse("2006-01-02", c.in)
		if err != nil {
			t.Fatal(err)
		}
		want, err := time.Parse("2006-01-02", c.want)
		if err != nil {
			t.Fatal(err)
		}
		got := mondayOf(in)
		if !got.Equal(want) {
			t.Errorf("mondayOf(%s) = %s，期望 %s", c.in, got.Format("2006-01-02"), c.want)
		}
	}
}

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

// TestEnsureAllWeekly_幂等且与迁移分区命名一致 是真机发现的真实 bug 的
// 回归测试：本组件的 event_outbox 表从建仓库起就是分区表（决策 54），
// 但 Module.Start 之前从没接过这个包——迁移只种了 4 周初始分区，第 5 周
// 起 INSERT 会因为找不到覆盖那个日期的分区而失败，Outbox 推送从此
// 静默停摆（所有新的 task.created/completed/cancelled 事件都发不出去，
// 且没有任何清晰的报错指向"分区没建"这个根因）。这条测试连跑两次
// ensureAllWeekly 验证幂等，且断言未来第 lookAheadWeeks 周的分区确实
// 建出来了——命名必须和迁移里种的分区名格式完全一致（"表名_YYYY_MM_DD"），
// 否则 to_regclass 查不到已存在的分区，会尝试新建同一段时间范围的分区，
// 撞上 PostgreSQL 分区范围不许重叠的报错（同 erp-inventory 设计计划 §9
// 第 8 条的既有教训）。
func TestEnsureAllWeekly_幂等且与迁移分区命名一致(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	role := "infra_workflow_rw"
	schema := "infra_workflow"

	if err := ensureAllWeekly(ctx, db, role, schema); err != nil {
		t.Fatalf("第一次 ensureAllWeekly 失败（很可能是分区命名与迁移不一致导致的重叠）：%v", err)
	}
	if err := ensureAllWeekly(ctx, db, role, schema); err != nil {
		t.Fatalf("第二次 ensureAllWeekly 失败，说明不是幂等的：%v", err)
	}

	// ⚠️ to_regclass 必须带 schema 前缀——db 这个连接没有把 schema 放进
	// search_path，裸表名会在默认 search_path 下解析成"不存在"，即使
	// 分区已经在 schema 里建好了。
	future := mondayOf(time.Now().UTC()).AddDate(0, 0, 7*lookAheadWeeks)
	for _, table := range weeklyPartitionedTables {
		name := schema + "." + table + "_" + future.Format("2006_01_02")
		var exists bool
		if err := db.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("期望未来第 %d 周的分区 %s 已建好，实际不存在", lookAheadWeeks, name)
		}
	}
}
