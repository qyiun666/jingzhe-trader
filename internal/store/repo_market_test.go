package store

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"jingzhe-trader/internal/model"
)

// TestCheckValuationCoverage 空/半空/部分有效截面的判定（agent_issues #13 防御）。
func TestCheckValuationCoverage(t *testing.T) {
	rows := func(n int) []model.Valuation {
		out := make([]model.Valuation, n)
		for i := range out {
			out[i] = model.Valuation{TsCode: fmt.Sprintf("6000%02d.SH", i), PETtm: 20, PB: 2}
		}
		return out
	}
	invalid := rows(200)
	for i := 50; i < len(invalid); i++ {
		invalid[i].PETtm = 0
		invalid[i].PB = 0
	}
	cases := []struct {
		name    string
		rows    []model.Valuation
		listed  int
		wantErr bool
	}{
		{"空截面", nil, 200, true},
		{"行数不足一半", rows(50), 200, true},
		{"有效值不足一半", invalid, 200, true},
		{"行数与有效值达标", rows(120), 120, false},
		{"无股票清单时只看有效值", rows(120), 0, false},
		{"小样本不启用比例阈值", rows(1), 4, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkValuationCoverage(tc.rows, tc.listed)
			if tc.wantErr && !errors.Is(err, ErrValuationSparse) {
				t.Errorf("期望 ErrValuationSparse，实际 %v", err)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("不应拒绝: %v", err)
			}
		})
	}
}

// TestSaveValuationKeepsOldOnSparse 残缺截面必须整批跳过，不得把已有估值盖空。
func TestSaveValuationKeepsOldOnSparse(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	ctx := context.Background()
	repo := s.MarketRepo()

	const listed = 120
	codes := make([]string, listed)
	for i := range codes {
		codes[i] = fmt.Sprintf("600%03d.SH", i)
		if err := repo.UpsertStockBasic(ctx, model.StockBasic{
			TsCode: codes[i], Name: "测试" + codes[i], ListStatus: "L",
		}); err != nil {
			t.Fatalf("落股票基础 %s 失败: %v", codes[i], err)
		}
	}
	if _, err := s.WriteDB().ExecContext(ctx,
		`UPDATE stock_basic SET val_date='20260101', turnover_rate=1, pe_ttm=10, pb=1, circ_mv_w=100`); err != nil {
		t.Fatalf("造旧估值失败: %v", err)
	}

	assertOld := func(label string) {
		t.Helper()
		var n, pe int
		if err := s.ReadDB().GetContext(ctx, &n,
			`SELECT COUNT(*) FROM stock_basic WHERE val_date='20260101' AND pe_ttm=10 AND pb=1`); err != nil {
			t.Fatalf("%s: 读旧估值失败: %v", label, err)
		}
		if n != len(codes) {
			t.Fatalf("%s: 旧估值应保留 %d 行，实际 %d 行", label, len(codes), n)
		}
		if err := s.ReadDB().GetContext(ctx, &pe,
			`SELECT COUNT(*) FROM stock_basic WHERE val_date='20261023'`); err != nil {
			t.Fatalf("%s: 读新日期失败: %v", label, err)
		}
		if pe != 0 {
			t.Fatalf("%s: 被拒绝的截面不应写入任何行，实际写了 %d 行", label, pe)
		}
	}

	if err := repo.SaveValuation(ctx, "20261023", nil); !errors.Is(err, ErrValuationSparse) {
		t.Fatalf("空截面应返回 ErrValuationSparse，实际 %v", err)
	}
	assertOld("空截面")

	sparse := make([]model.Valuation, 10)
	for i := range sparse {
		sparse[i] = model.Valuation{TsCode: codes[i], PETtm: 20, PB: 2}
	}
	if err := repo.SaveValuation(ctx, "20261023", sparse); !errors.Is(err, ErrValuationSparse) {
		t.Fatalf("半空截面应返回 ErrValuationSparse，实际 %v", err)
	}
	assertOld("半空截面")

	invalid := make([]model.Valuation, listed)
	for i := range invalid {
		invalid[i] = model.Valuation{TsCode: codes[i]}
		if i < 10 {
			invalid[i].PETtm = 20
			invalid[i].PB = 2
		}
	}
	if err := repo.SaveValuation(ctx, "20261023", invalid); !errors.Is(err, ErrValuationSparse) {
		t.Fatalf("有效值不足应返回 ErrValuationSparse，实际 %v", err)
	}
	assertOld("有效值不足")

	good := make([]model.Valuation, listed)
	for i := range good {
		good[i] = model.Valuation{TsCode: codes[i], PETtm: float64(20 + i), PB: float64(2 + i)}
	}
	if err := repo.SaveValuation(ctx, "20261023", good); err != nil {
		t.Fatalf("完整截面写入失败: %v", err)
	}
	var n int
	if err := s.ReadDB().GetContext(ctx, &n,
		`SELECT COUNT(*) FROM stock_basic WHERE val_date='20261023' AND pe_ttm>0 AND pb>0`); err != nil {
		t.Fatal(err)
	}
	if n != len(codes) {
		t.Fatalf("完整截面应写入 %d 行，实际 %d 行", len(codes), n)
	}
}
