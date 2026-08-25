package indicator

// 适配层测试: 委托 go-talib 后数值与自研黄金实现完全一致 (同一输入, 委托路径 vs 自研回退路径)
// 守护: SMA/WMA/BOLL 委托不改变行为; 扩展指标遵循等长+NaN契约

import (
	"math"
	"testing"

	talib "github.com/markcheno/go-talib"
)

// delegateInput 委托路径测试数据 (无 NaN, 走 go-talib)
var delegateInput = []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}

func TestSMADelegateMatchesLocal(t *testing.T) {
	for _, period := range []int{1, 3, 5, len(delegateInput)} {
		got := SMA(delegateInput, period)
		want := smaLocal(delegateInput, period)
		if !approxSliceEqual(got, want) {
			t.Errorf("SMA(period=%d) 委托与自研不一致:\n got=%v\nwant=%v", period, got, want)
		}
	}
	// 参数越界: period>n 全 NaN
	if got := SMA(delegateInput, 99); !allNaN(got) {
		t.Error("SMA(period>n) 期望全 NaN")
	}
}

func TestWMADelegateMatchesLocal(t *testing.T) {
	for _, period := range []int{1, 3, 5, len(delegateInput)} {
		got := WMA(delegateInput, period)
		want := wmaLocal(delegateInput, period)
		if !approxSliceEqual(got, want) {
			t.Errorf("WMA(period=%d) 委托与自研不一致:\n got=%v\nwant=%v", period, got, want)
		}
	}
}

func TestBollDelegateMatchesLocal(t *testing.T) {
	for _, period := range []int{3, 5} {
		got := Boll(delegateInput, period, 2.0)
		want := bollLocal(delegateInput, period, 2.0)
		if !approxSliceEqual(got.Upper, want.Upper) || !approxSliceEqual(got.Middle, want.Middle) || !approxSliceEqual(got.Lower, want.Lower) {
			t.Errorf("Boll(period=%d) 委托与自研不一致", period)
		}
	}
}

func TestDelegateFallbackOnNaN(t *testing.T) {
	// 输入含 NaN 时回退自研, 输出与自研完全一致 (talib 对 NaN 传播不同, 不可委托)
	values := []float64{1, math.NaN(), 3, 4, 5, 6, 7, 8, 9}
	if got := SMA(values, 3); !approxSliceEqual(got, smaLocal(values, 3)) {
		t.Error("SMA 含 NaN 回退与自研不一致")
	}
	if got := WMA(values, 3); !approxSliceEqual(got, wmaLocal(values, 3)) {
		t.Error("WMA 含 NaN 回退与自研不一致")
	}
	got := Boll(values, 3, 2.0)
	want := bollLocal(values, 3, 2.0)
	if !approxSliceEqual(got.Upper, want.Upper) || !approxSliceEqual(got.Middle, want.Middle) || !approxSliceEqual(got.Lower, want.Lower) {
		t.Error("Boll 含 NaN 回退与自研不一致")
	}
}

// extInput 扩展指标测试数据 (40 根K线, 满足 ADX lookback=2*period-1)
var extHigh = []float64{10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32, 33, 34, 35, 36, 37, 38, 39, 40, 41, 42, 43, 44, 45, 46, 47, 48, 49}
var extLow = []float64{8, 9, 9, 10, 11, 12, 13, 13, 14, 15, 16, 17, 18, 18, 19, 20, 21, 22, 23, 24, 24, 25, 26, 27, 28, 29, 30, 30, 31, 32, 33, 34, 35, 36, 37, 38, 39, 40, 40, 41}
var extClose = []float64{9, 10, 11, 12, 13, 14, 15, 15, 16, 17, 18, 19, 20, 21, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32, 33, 34, 35, 36, 37, 38, 39, 40, 41, 42, 43, 44, 45, 46}
var extVol = []float64{100, 200, 150, 300, 250, 400, 350, 500, 450, 600, 550, 700, 650, 800, 750, 900, 850, 1000, 950, 1100, 1050, 1200, 1150, 1300, 1250, 1400, 1350, 1500, 1450, 1600, 1550, 1700, 1650, 1800, 1750, 1900, 1850, 2000, 1950, 2100}

// extNaN 校验: 有效区与 talib 原始输出一致, 无效区为 NaN
func TestExtADXContract(t *testing.T) {
	n := len(extClose)
	got := ADX(extHigh, extLow, extClose, 14)
	if len(got) != n {
		t.Fatalf("ADX 输出长度 = %d, 期望 %d", len(got), n)
	}
	// talib lookback = 2*period-1, 前 2*period-1 个为 NaN (padTo 填充)
	for i := 0; i < 2*14-1; i++ {
		if !math.IsNaN(got[i]) {
			t.Errorf("ADX[%d] 期望 NaN, got %v", i, got[i])
		}
	}
	raw := talib.Adx(extHigh, extLow, extClose, 14)
	for i := 2*14 - 1; i < n; i++ {
		if !approxEqual(got[i], raw[i]) {
			t.Errorf("ADX[%d] = %v, 期望 %v (与 talib 一致)", i, got[i], raw[i])
		}
	}
	// 数据不足返回全 NaN
	if got := ADX(extHigh, extLow, extClose, 99); !allNaN(got) {
		t.Error("ADX 数据不足期望全 NaN")
	}
}

func TestExtCCIAndROCContract(t *testing.T) {
	n := len(extClose)
	cci := CCI(extHigh, extLow, extClose, 14)
	if len(cci) != n || !math.IsNaN(cci[12]) || math.IsNaN(cci[13]) {
		t.Errorf("CCI 契约不符: len=%d, [12]=%v, [13]=%v", len(cci), cci[12], cci[13])
	}
	rawCCI := talib.Cci(extHigh, extLow, extClose, 14)
	if !approxEqual(cci[13], rawCCI[13]) {
		t.Errorf("CCI[13] = %v, 期望 %v", cci[13], rawCCI[13])
	}

	roc := ROC(extClose, 12)
	if len(roc) != n || !math.IsNaN(roc[11]) || math.IsNaN(roc[12]) {
		t.Errorf("ROC 契约不符: len=%d, [11]=%v, [12]=%v", len(roc), roc[11], roc[12])
	}
	rawROC := talib.Roc(extClose, 12)
	if !approxEqual(roc[12], rawROC[12]) {
		t.Errorf("ROC[12] = %v, 期望 %v", roc[12], rawROC[12])
	}
}

func TestExtOBVContract(t *testing.T) {
	n := len(extClose)
	got := OBV(extClose, extVol)
	if len(got) != n {
		t.Fatalf("OBV 输出长度 = %d, 期望 %d", len(got), n)
	}
	raw := talib.Obv(extClose, extVol)
	for i := 0; i < n; i++ {
		if !approxEqual(got[i], raw[i]) {
			t.Errorf("OBV[%d] = %v, 期望 %v", i, got[i], raw[i])
		}
	}
	// 长度不匹配返回全 NaN
	if got := OBV(extClose, extVol[:5]); !allNaN(got) {
		t.Error("OBV 长度不匹配期望全 NaN")
	}
}

func TestExtSTOCHContract(t *testing.T) {
	n := len(extClose)
	got := STOCH(extHigh, extLow, extClose, 9, 3, 3)
	if len(got.K) != n || len(got.D) != n {
		t.Fatalf("STOCH 输出长度 = %d/%d, 期望 %d", len(got.K), len(got.D), n)
	}
	// 有效起点 = 9+3+3-3 = 12
	if !math.IsNaN(got.K[11]) || math.IsNaN(got.K[12]) {
		t.Errorf("STOCH.K 契约不符: [11]=%v, [12]=%v", got.K[11], got.K[12])
	}
	rawK, rawD := talib.Stoch(extHigh, extLow, extClose, 9, 3, talib.SMA, 3, talib.SMA)
	for i := 12; i < n; i++ {
		if !approxEqual(got.K[i], rawK[i]) || !approxEqual(got.D[i], rawD[i]) {
			t.Errorf("STOCH[%d] K=%v D=%v, 期望 K=%v D=%v", i, got.K[i], got.D[i], rawK[i], rawD[i])
		}
	}
}

func TestExtInputValidation(t *testing.T) {
	// 输入含 NaN 返回全 NaN (talib 对 NaN 传播不确定, 保守处理)
	nan := append([]float64{}, extClose...)
	nan[3] = math.NaN()
	if got := ROC(nan, 12); !allNaN(got) {
		t.Error("ROC 含 NaN 期望全 NaN")
	}
	if got := OBV(nan, extVol); !allNaN(got) {
		t.Error("OBV 含 NaN 期望全 NaN")
	}
	if got := CCI(extHigh, extLow, nan, 14); !allNaN(got) {
		t.Error("CCI 含 NaN 期望全 NaN")
	}
}
