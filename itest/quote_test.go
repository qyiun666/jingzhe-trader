package itest

import (
	"context"
	"testing"
	"time"

	"jingzhe-trader/internal/quote"
)

// TestQuoteSource 实时行情接口：要求"每个请求标的都拿到正价"，否则整体报错。
//
// 腾讯备用源与缓存兜底已删除，所以这里同时验证反向：批次里混入取不到价的标的时
// 必须失败，而不是静默少一个 key —— 上层会把"没拿到价"当成"这只票没触发止损"。
func TestQuoteSource(t *testing.T) {
	requireEnvOnly(t)
	src := quote.NewGotdxSource()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	codes := []string{"600519.SH", "000001.SZ", "300750.SZ"}
	qs, err := src.Fetch(ctx, codes)
	if err != nil {
		t.Fatalf("真实行情获取失败: %v", err)
	}
	for _, c := range codes {
		q, ok := qs[c]
		if !ok {
			t.Errorf("%s 没有出现在返回里（契约要求全覆盖）", c)
			continue
		}
		if q.Price <= 0 {
			t.Errorf("%s 最新价为 0: %+v", c, q)
		}
		if q.Source != "gotdx" {
			t.Errorf("%s Source=%q，期望 gotdx", c, q.Source)
		}
	}
	t.Log(describe("标的数", len(qs), "贵州茅台", qs["600519.SH"].Price.Float()))

	// 注意别用 999999.SH：它在 TDX 里是上证综指，确实有报价。
	// 000000.SZ 实测返回一条对不上的记录（code=600839、close=0），必须算失败。
	if _, err := src.Fetch(ctx, []string{"000000.SZ"}); err == nil {
		t.Error("取不到有效报价的代码应当报错，实际静默返回（持仓里有它就会整天不判止损）")
	}
	if _, err := src.Fetch(ctx, []string{"600519.SH", "000000.SZ"}); err == nil {
		t.Error("批次里混入取不到价的标的时应当整体失败，不允许部分成功")
	}
}

// TestQuoteSourceEmptyBatch 空批次是合法输入（无持仓时的盘中扫描就走这条路）。
func TestQuoteSourceEmptyBatch(t *testing.T) {
	requireEnvOnly(t)
	qs, err := quote.NewGotdxSource().Fetch(t.Context(), nil)
	mustNoErr(t, "空批次 Fetch", err)
	if len(qs) != 0 {
		t.Errorf("空批次应返回空集合，实际 %d 条", len(qs))
	}
}
