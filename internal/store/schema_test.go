package store

import (
	"context"
	"testing"

	"github.com/jmoiron/sqlx"
	"jingzhe-trader/internal/model"
)

// wantTables 库的完整表清单，每张表点名读者。
//
// 加一张表必须同时在这里写清它凭什么不能并进现有表（粒度或保留窗口不同），
// 这条断言就是"又把过程状态建成一张表"的第一道关。
var wantTables = []string{
	"config_kv",    // 配置 + 键目录 | suspend:<日期> | 现金锚点
	"daily_bar",    // 选股因子、指数 MA60、持仓市值与一手价
	"order_ticket", // 晨会/盘中邮件、账本现金推算、决策复盘
	"position",     // 卖出信号、风控仓位、资产合计
	"run_trace",    // 调度补跑判定、自检、MCP get_logs、日报
	"stock_basic",  // 选股漏斗的横截面（静态属性 + 当日估值）
	"trade_cal",    // 交易日判定、有效期、因子窗口取日
}

// goneTables 已按判据折叠或废弃的老表，任何一张都不许复活。
var goneTables = []string{
	"daily_basic", "suspend_d", "llm_call",
	"job_run", "agent_alert", "action_log", "mail_outbox", "fill", "index_daily", "schema_version",
}

// TestSchemaShape 建表结果必须与 schemaDDL 一致：表数、表名顺序、老表不复活。
func TestSchemaShape(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()

	var got []string
	if err := s.readDB.Select(&got,
		`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`); err != nil {
		t.Fatalf("读取表清单失败: %v", err)
	}
	if len(got) != len(wantTables) {
		t.Fatalf("表数 = %d %v，期望 %d %v", len(got), got, len(wantTables), wantTables)
	}
	for i := range wantTables {
		if got[i] != wantTables[i] {
			t.Fatalf("第 %d 张表 = %s，期望 %s", i+1, got[i], wantTables[i])
		}
	}
	for _, gone := range goneTables {
		if hasTable(t, s.readDB, gone) {
			t.Errorf("%s 已按粒度/读者判据折叠进别的表，不该再建出来", gone)
		}
	}
}

func hasTable(t *testing.T, db *sqlx.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name); err != nil {
		t.Fatalf("探测表 %s 失败: %v", name, err)
	}
	return n > 0
}

// TestSchemaColumnsMirrorDDL 列级镜像必须与 schemaDDL 实际建出的列一字不差。
//
// SchemaColumns 是手抄的清单（DDL 是真相源），没有这条断言，清单自身漂移就会让
// 列级审计与启动回填建立在错误期望上——比没有清单更糟（假绿换了一处发生）。
// 加列、删列、改列名都会在这里第一秒被抓住，而不是等线上 SQL 炸。
func TestSchemaColumnsMirrorDDL(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()

	// 清单覆盖的表集合 == SchemaTables，一张不落。
	covered := make(map[string]bool)
	for _, tc := range SchemaColumns {
		if covered[tc.Table] {
			t.Fatalf("SchemaColumns 里 %s 出现两次", tc.Table)
		}
		covered[tc.Table] = true
	}
	if len(covered) != len(SchemaTables) {
		t.Fatalf("列清单覆盖 %d 张表，SchemaTables 有 %d 张", len(covered), len(SchemaTables))
	}
	for _, tab := range SchemaTables {
		if !covered[tab] {
			t.Errorf("表 %s 没有列清单条目", tab)
		}
	}

	// 每张表的列名集合与库内实际一致（双向差集都要为空）。
	for _, tc := range SchemaColumns {
		got, err := s.tableColumns(context.Background(), tc.Table)
		if err != nil {
			t.Fatalf("读取 %s 列失败: %v", tc.Table, err)
		}
		want := make(map[string]bool, len(tc.Columns))
		for _, c := range tc.Columns {
			want[c.Name] = true
		}
		for name := range want {
			if !got[name] {
				t.Errorf("列清单写了 %s.%s，但 DDL 没建出来（镜像漂移）", tc.Table, name)
			}
		}
		for name := range got {
			if !want[name] {
				t.Errorf("DDL 建出了 %s.%s，但列清单没写（镜像漂移）", tc.Table, name)
			}
		}
	}
}

// TestStockBasicCarriesValuation 估值截面并入 stock_basic 后的三条硬约定：
// 按 val_date 认日期、只盖已有行、静态属性与估值互不覆盖。
func TestStockBasicCarriesValuation(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	ctx := context.Background()
	rc := s.MarketRepo()

	for _, code := range []string{"600000.SH", "600001.SH"} {
		if err := rc.UpsertStockBasic(ctx, model.StockBasic{
			TsCode: code, Name: "测试" + code, Industry: "银行", ListDate: "19990101", ListStatus: "L",
		}); err != nil {
			t.Fatalf("写入股票基础失败: %v", err)
		}
	}
	if err := rc.SaveValuation(ctx, "20260901",
		[]model.Valuation{{TsCode: "600000.SH", TurnoverRate: 1.5, PETtm: 6.1, PB: 0.6, CircMvW: 3000000}}); err != nil {
		t.Fatalf("写入估值截面失败: %v", err)
	}
	n, err := rc.CountValuation(ctx, "20260901")
	if err != nil {
		t.Fatalf("统计估值截面失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("20260901 估值截面 = %d 行，期望 1", n)
	}
	if n, _ = rc.CountValuation(ctx, "20260902"); n != 0 {
		t.Fatalf("未同步的日期不该有截面，实际 %d 行", n)
	}

	// 只盖已有行：不在 universe 里的代码不建行（那行没有读者）
	if err := rc.SaveValuation(ctx, "20260902", []model.Valuation{{TsCode: "900999.SH"}}); err != nil {
		t.Fatalf("写入陌生代码估值失败: %v", err)
	}
	if n, _ = rc.CountValuation(ctx, "20260902"); n != 0 {
		t.Fatalf("陌生代码不该被建出行来，实际 %d 行", n)
	}

	// 静态属性再同步一次，不能把已写入的估值列清零（两条写路径各管各的列）
	if err := rc.UpsertStockBasic(ctx, model.StockBasic{
		TsCode: "600000.SH", Name: "测试改后名", Industry: "银行", ListDate: "19990101", ListStatus: "L",
	}); err != nil {
		t.Fatalf("二次写入股票基础失败: %v", err)
	}
	if n, _ = rc.CountValuation(ctx, "20260901"); n != 1 {
		t.Fatalf("覆盖静态属性不该抹掉估值截面，实际剩 %d 行", n)
	}
	stocks, err := s.ScreenRepo().LiveStocks(ctx)
	if err != nil {
		t.Fatalf("读取在市股票失败: %v", err)
	}
	for _, stk := range stocks {
		if stk.TsCode != "600000.SH" {
			continue
		}
		if stk.Name != "测试改后名" || stk.PETtm != 6.1 || stk.ValDate != "20260901" {
			t.Errorf("同一行读到 name=%q pe=%.1f val_date=%q，期望 测试改后名/6.1/20260901",
				stk.Name, stk.PETtm, stk.ValDate)
		}
	}
}
