package indicator

import (
	"math"
	"testing"
)

// approxEqual 判断两个浮点数是否近似相等 (相对误差 < 1e-9)
func approxEqual(a, b float64) bool {
	if math.IsNaN(a) && math.IsNaN(b) {
		return true
	}
	if math.IsNaN(a) || math.IsNaN(b) {
		return false
	}
	return math.Abs(a-b) < 1e-9
}

// approxSliceEqual 判断两个切片是否近似相等
func approxSliceEqual(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !approxEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

// allNaN 判断切片是否全为 NaN
func allNaN(a []float64) bool {
	for _, v := range a {
		if !math.IsNaN(v) {
			return false
		}
	}
	return true
}

// ================ helpers.go 测试 ================

func TestMean(t *testing.T) {
	tests := []struct {
		name   string
		values []float64
		want   float64
	}{
		{"基本", []float64{1, 2, 3, 4, 5}, 3},
		{"单元素", []float64{42}, 42},
		{"空", []float64{}, math.NaN()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Mean(tt.values)
			if !approxEqual(got, tt.want) {
				t.Errorf("Mean() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestVarianceAndStdDev(t *testing.T) {
	// values = [1,2,3,4,5], mean=3
	// variance = ((1-3)^2+(2-3)^2+(3-3)^2+(4-3)^2+(5-3)^2)/5 = (4+1+0+1+4)/5 = 2
	// stddev = sqrt(2)
	values := []float64{1, 2, 3, 4, 5}
	v := Variance(values)
	if !approxEqual(v, 2.0) {
		t.Errorf("Variance() = %v, want 2.0", v)
	}
	sd := StdDev(values)
	if !approxEqual(sd, math.Sqrt(2)) {
		t.Errorf("StdDev() = %v, want %v", sd, math.Sqrt(2))
	}
	// 空输入
	if !math.IsNaN(Variance(nil)) {
		t.Error("Variance(nil) should be NaN")
	}
	if !math.IsNaN(StdDev(nil)) {
		t.Error("StdDev(nil) should be NaN")
	}
}

func TestMaxMin(t *testing.T) {
	values := []float64{3, 1, 4, 1, 5, 9, 2, 6}
	if !approxEqual(Max(values), 9) {
		t.Errorf("Max() = %v, want 9", Max(values))
	}
	if !approxEqual(Min(values), 1) {
		t.Errorf("Min() = %v, want 1", Min(values))
	}
	if !math.IsNaN(Max(nil)) || !math.IsNaN(Min(nil)) {
		t.Error("Max/Min(nil) should be NaN")
	}
}

func TestHighestLowest(t *testing.T) {
	values := []float64{3, 1, 4, 1, 5}
	// Highest period=3
	h := Highest(values, 3)
	// h[0]=NaN, h[1]=NaN, h[2]=max(3,1,4)=4, h[3]=max(1,4,1)=4, h[4]=max(4,1,5)=5
	wantH := []float64{math.NaN(), math.NaN(), 4, 4, 5}
	if !approxSliceEqual(h, wantH) {
		t.Errorf("Highest() = %v, want %v", h, wantH)
	}
	// Lowest period=3
	l := Lowest(values, 3)
	// l[0]=NaN, l[1]=NaN, l[2]=min(3,1,4)=1, l[3]=min(1,4,1)=1, l[4]=min(4,1,5)=1
	wantL := []float64{math.NaN(), math.NaN(), 1, 1, 1}
	if !approxSliceEqual(l, wantL) {
		t.Errorf("Lowest() = %v, want %v", l, wantL)
	}
	// period 不合法
	if !allNaN(Highest(values, 0)) {
		t.Error("Highest(period=0) should be all NaN")
	}
	if !allNaN(Lowest(values, 0)) {
		t.Error("Lowest(period=0) should be all NaN")
	}
}

// ================ ma.go 测试 ================

func TestSMA(t *testing.T) {
	values := []float64{1, 2, 3, 4, 5}
	// period=3: [NaN, NaN, 2, 3, 4]
	got := SMA(values, 3)
	want := []float64{math.NaN(), math.NaN(), 2, 3, 4}
	if !approxSliceEqual(got, want) {
		t.Errorf("SMA() = %v, want %v", got, want)
	}
	// period=1: 原样返回
	got1 := SMA(values, 1)
	if !approxSliceEqual(got1, values) {
		t.Errorf("SMA(period=1) = %v, want %v", got1, values)
	}
	// period 不合法
	if !allNaN(SMA(values, 0)) {
		t.Error("SMA(period=0) should be all NaN")
	}
	// period > len
	if !allNaN(SMA(values, 10)) {
		t.Error("SMA(period=10) should be all NaN")
	}
}

func TestEMA(t *testing.T) {
	values := []float64{1, 2, 3, 4, 5}
	// period=3, k=2/(3+1)=0.5
	// EMA[2] = SMA(1,2,3) = 2
	// EMA[3] = 2*0.5 + 4*0.5 = 3
	// EMA[4] = 3*0.5 + 5*0.5 = 4
	got := EMA(values, 3)
	want := []float64{math.NaN(), math.NaN(), 2, 3, 4}
	if !approxSliceEqual(got, want) {
		t.Errorf("EMA() = %v, want %v", got, want)
	}
	// period 不合法
	if !allNaN(EMA(values, 0)) {
		t.Error("EMA(period=0) should be all NaN")
	}
}

func TestWMA(t *testing.T) {
	values := []float64{1, 2, 3, 4, 5}
	// period=3, weightSum=6
	// WMA[2] = (1*1 + 2*2 + 3*3)/6 = 14/6
	// WMA[3] = (2*1 + 3*2 + 4*3)/6 = 20/6
	// WMA[4] = (3*1 + 4*2 + 5*3)/6 = 26/6
	got := WMA(values, 3)
	want := []float64{math.NaN(), math.NaN(), 14.0 / 6, 20.0 / 6, 26.0 / 6}
	if !approxSliceEqual(got, want) {
		t.Errorf("WMA() = %v, want %v", got, want)
	}
	// period 不合法
	if !allNaN(WMA(values, 0)) {
		t.Error("WMA(period=0) should be all NaN")
	}
}

// ================ macd.go 测试 ================

func TestMACD(t *testing.T) {
	// 使用较长序列测试 MACD
	values := []float64{
		10, 11, 12, 11, 10, 9, 10, 11, 12, 13,
		14, 13, 12, 13, 14, 15, 16, 15, 14, 15,
		16, 17, 18, 17, 16, 17, 18, 19, 20, 19,
	}
	result := MACD(values, 12, 26, 9)

	// 检查长度
	if len(result.DIF) != len(values) || len(result.DEA) != len(values) || len(result.Histogram) != len(values) {
		t.Fatalf("MACD 结果长度不匹配: DIF=%d, DEA=%d, Hist=%d, input=%d",
			len(result.DIF), len(result.DEA), len(result.Histogram), len(values))
	}

	// DIF 从 slow-1=25 开始有效
	for i := 0; i < 25; i++ {
		if !math.IsNaN(result.DIF[i]) {
			t.Errorf("DIF[%d] 应为 NaN, 得到 %v", i, result.DIF[i])
		}
	}
	// DIF[25] 应有效
	if math.IsNaN(result.DIF[25]) {
		t.Error("DIF[25] 不应为 NaN")
	}

	// DEA 从 slow-1+signal-1 = 25+8 = 33 开始有效
	// 但数组长度只有 30, 所以 DEA 全为 NaN
	if len(values) < 26+9-1 {
		// 数据不足以产生 DEA, 验证 DEA 全 NaN
		if !allNaN(result.DEA) {
			t.Error("数据不足时 DEA 应全为 NaN")
		}
	}

	// 验证 Histogram = (DIF - DEA) * 2
	for i := range result.Histogram {
		if math.IsNaN(result.DEA[i]) {
			if !math.IsNaN(result.Histogram[i]) {
				t.Errorf("Histogram[%d] 应为 NaN", i)
			}
		} else {
			expected := (result.DIF[i] - result.DEA[i]) * 2
			if !approxEqual(result.Histogram[i], expected) {
				t.Errorf("Histogram[%d] = %v, want %v", i, result.Histogram[i], expected)
			}
		}
	}
}

func TestMACDInvalidParams(t *testing.T) {
	values := []float64{1, 2, 3, 4, 5}
	// fast >= slow 应返回全 NaN
	result := MACD(values, 5, 3, 2)
	if !allNaN(result.DIF) || !allNaN(result.DEA) || !allNaN(result.Histogram) {
		t.Error("fast>=slow 时应返回全 NaN")
	}
	// period 不合法
	result2 := MACD(values, 0, 3, 2)
	if !allNaN(result2.DIF) {
		t.Error("fast=0 时应返回全 NaN")
	}
}

// ================ rsi.go 测试 ================

func TestRSI(t *testing.T) {
	// 全上涨序列, RSI 应为 100
	up := []float64{1, 2, 3, 4, 5}
	rsiUp := RSI(up, 3)
	// RSI[0..2]=NaN, RSI[3]=100, RSI[4]=100
	for i := 0; i < 3; i++ {
		if !math.IsNaN(rsiUp[i]) {
			t.Errorf("RSI[%d] 应为 NaN, 得到 %v", i, rsiUp[i])
		}
	}
	if !approxEqual(rsiUp[3], 100) {
		t.Errorf("RSI[3] = %v, want 100", rsiUp[3])
	}
	if !approxEqual(rsiUp[4], 100) {
		t.Errorf("RSI[4] = %v, want 100", rsiUp[4])
	}

	// 全下跌序列, RSI 应为 0
	down := []float64{5, 4, 3, 2, 1}
	rsiDown := RSI(down, 3)
	if !approxEqual(rsiDown[3], 0) {
		t.Errorf("RSI[3] = %v, want 0", rsiDown[3])
	}
	if !approxEqual(rsiDown[4], 0) {
		t.Errorf("RSI[4] = %v, want 0", rsiDown[4])
	}
}

func TestRSIMixed(t *testing.T) {
	// [1,2,3,4,3,2,1] period=3
	// deltas: +1,+1,+1,+1,-1,-1,-1
	// gains:  [0,1,1,1,1,0,0,0] (index 0..7, but only 1..7 used)
	// losses: [0,0,0,0,0,0,1,1,1] -- wait let me recompute
	// values = [1,2,3,4,3,2,1]
	// delta[1]=2-1=1 (gain=1, loss=0)
	// delta[2]=3-2=1 (gain=1, loss=0)
	// delta[3]=4-3=1 (gain=1, loss=0)
	// delta[4]=3-4=-1 (gain=0, loss=1)
	// delta[5]=2-3=-1 (gain=0, loss=1)
	// delta[6]=1-2=-1 (gain=0, loss=1)
	// period=3:
	// first avgGain = (gains[1]+gains[2]+gains[3])/3 = 3/3 = 1
	// first avgLoss = 0
	// RSI[3] = 100
	// avgGain[4] = (1*2 + 0)/3 = 2/3
	// avgLoss[4] = (0*2 + 1)/3 = 1/3
	// RS = 2, RSI[4] = 100 - 100/3 = 66.666...
	values := []float64{1, 2, 3, 4, 3, 2, 1}
	rsi := RSI(values, 3)
	if !approxEqual(rsi[3], 100) {
		t.Errorf("RSI[3] = %v, want 100", rsi[3])
	}
	want4 := 100.0 - 100.0/3.0
	if !approxEqual(rsi[4], want4) {
		t.Errorf("RSI[4] = %v, want %v", rsi[4], want4)
	}
}

func TestRSIInvalid(t *testing.T) {
	if !allNaN(RSI([]float64{1, 2}, 3)) {
		t.Error("数据不足时 RSI 应全为 NaN")
	}
	if !allNaN(RSI([]float64{1, 2, 3}, 0)) {
		t.Error("period=0 时 RSI 应全为 NaN")
	}
}

// ================ boll.go 测试 ================

func TestBoll(t *testing.T) {
	values := []float64{1, 2, 3, 4, 5}
	result := Boll(values, 3, 2.0)

	// 中轨 = SMA(3) = [NaN, NaN, 2, 3, 4]
	wantMid := []float64{math.NaN(), math.NaN(), 2, 3, 4}
	if !approxSliceEqual(result.Middle, wantMid) {
		t.Errorf("Middle = %v, want %v", result.Middle, wantMid)
	}

	// 标准差: [1,2,3] mean=2, var=2/3, sd=sqrt(2/3)
	sd := math.Sqrt(2.0 / 3.0)
	wantUpper := []float64{math.NaN(), math.NaN(), 2 + 2*sd, 3 + 2*sd, 4 + 2*sd}
	wantLower := []float64{math.NaN(), math.NaN(), 2 - 2*sd, 3 - 2*sd, 4 - 2*sd}
	if !approxSliceEqual(result.Upper, wantUpper) {
		t.Errorf("Upper = %v, want %v", result.Upper, wantUpper)
	}
	if !approxSliceEqual(result.Lower, wantLower) {
		t.Errorf("Lower = %v, want %v", result.Lower, wantLower)
	}

	// 验证上下轨关于中轨对称
	for i := range values {
		if !math.IsNaN(result.Middle[i]) {
			mid := (result.Upper[i] + result.Lower[i]) / 2
			if !approxEqual(mid, result.Middle[i]) {
				t.Errorf("位置 %d: 上下轨中点 %v != 中轨 %v", i, mid, result.Middle[i])
			}
		}
	}
}

func TestBollInvalid(t *testing.T) {
	result := Boll([]float64{1, 2}, 0, 2.0)
	if !allNaN(result.Upper) || !allNaN(result.Middle) || !allNaN(result.Lower) {
		t.Error("period=0 时 Boll 应全为 NaN")
	}
}

// ================ kdj.go 测试 ================

func TestKDJ(t *testing.T) {
	highs := []float64{3, 5, 7}
	lows := []float64{1, 2, 3}
	closes := []float64{2, 4, 6}
	period := 3

	result := KDJ(highs, lows, closes, period)

	// 前 period-1=2 个为 NaN
	for i := 0; i < 2; i++ {
		if !math.IsNaN(result.K[i]) || !math.IsNaN(result.D[i]) || !math.IsNaN(result.J[i]) {
			t.Errorf("KDJ[%d] 应为 NaN", i)
		}
	}

	// i=2:
	// highestHigh = max(3,5,7) = 7
	// lowestLow = min(1,2,3) = 1
	// RSV = (6-1)/(7-1)*100 = 500/6 = 83.3333
	rsv := (6.0 - 1.0) / (7.0 - 1.0) * 100
	// K = 2/3*50 + 1/3*RSV
	wantK := 2.0/3.0*50 + 1.0/3.0*rsv
	// D = 2/3*50 + 1/3*K
	wantD := 2.0/3.0*50 + 1.0/3.0*wantK
	// J = 3*K - 2*D
	wantJ := 3.0*wantK - 2.0*wantD

	if !approxEqual(result.K[2], wantK) {
		t.Errorf("K[2] = %v, want %v", result.K[2], wantK)
	}
	if !approxEqual(result.D[2], wantD) {
		t.Errorf("D[2] = %v, want %v", result.D[2], wantD)
	}
	if !approxEqual(result.J[2], wantJ) {
		t.Errorf("J[2] = %v, want %v", result.J[2], wantJ)
	}

	// 验证 J = 3K - 2D
	for i := range result.K {
		if !math.IsNaN(result.K[i]) {
			wantJ := 3*result.K[i] - 2*result.D[i]
			if !approxEqual(result.J[i], wantJ) {
				t.Errorf("J[%d] = %v, want %v (3K-2D)", i, result.J[i], wantJ)
			}
		}
	}
}

func TestKDJInvalid(t *testing.T) {
	// 长度不一致
	highs := []float64{1, 2, 3}
	lows := []float64{1, 2}
	closes := []float64{1, 2, 3}
	result := KDJ(highs, lows, closes, 2)
	if !allNaN(result.K) || !allNaN(result.D) || !allNaN(result.J) {
		t.Error("长度不一致时 KDJ 应全为 NaN")
	}
	// period 不合法
	result2 := KDJ(highs, lows, highs, 0)
	if !allNaN(result2.K) {
		t.Error("period=0 时 KDJ 应全为 NaN")
	}
}

// ================ atr.go 测试 ================

func TestATR(t *testing.T) {
	highs := []float64{3, 5, 7}
	lows := []float64{1, 2, 3}
	closes := []float64{2, 4, 6}
	period := 2

	got := ATR(highs, lows, closes, period)

	// TR[0] = 3-1 = 2
	// TR[1] = max(5-2, |5-2|, |2-2|) = max(3,3,0) = 3
	// TR[2] = max(7-3, |7-4|, |3-4|) = max(4,3,1) = 4
	// ATR[0] = NaN (period=2, 前1个NaN)
	// ATR[1] = SMA(TR[0],TR[1]) = (2+3)/2 = 2.5
	// ATR[2] = (2.5*1 + 4)/2 = 3.25
	want := []float64{math.NaN(), 2.5, 3.25}
	if !approxSliceEqual(got, want) {
		t.Errorf("ATR() = %v, want %v", got, want)
	}
}

func TestATRInvalid(t *testing.T) {
	highs := []float64{1, 2, 3}
	lows := []float64{1, 2, 3}
	closes := []float64{1, 2}
	// 长度不一致
	if !allNaN(ATR(highs, lows, closes, 2)) {
		t.Error("长度不一致时 ATR 应全为 NaN")
	}
	// period 不合法
	if !allNaN(ATR(highs, lows, highs, 0)) {
		t.Error("period=0 时 ATR 应全为 NaN")
	}
}

// ================ 综合测试 ================

func TestOutputLength(t *testing.T) {
	values := make([]float64, 50)
	for i := range values {
		values[i] = float64(i) + 1
	}

	// 所有单序列指标的输出长度应与输入相同
	if len(SMA(values, 10)) != 50 {
		t.Error("SMA 输出长度不匹配")
	}
	if len(EMA(values, 10)) != 50 {
		t.Error("EMA 输出长度不匹配")
	}
	if len(WMA(values, 10)) != 50 {
		t.Error("WMA 输出长度不匹配")
	}
	if len(RSI(values, 10)) != 50 {
		t.Error("RSI 输出长度不匹配")
	}

	highs := make([]float64, 50)
	lows := make([]float64, 50)
	for i := range values {
		highs[i] = float64(i) + 2
		lows[i] = float64(i)
	}

	boll := Boll(values, 10, 2.0)
	if len(boll.Upper) != 50 || len(boll.Middle) != 50 || len(boll.Lower) != 50 {
		t.Error("Boll 输出长度不匹配")
	}

	kdj := KDJ(highs, lows, values, 9)
	if len(kdj.K) != 50 || len(kdj.D) != 50 || len(kdj.J) != 50 {
		t.Error("KDJ 输出长度不匹配")
	}

	if len(ATR(highs, lows, values, 14)) != 50 {
		t.Error("ATR 输出长度不匹配")
	}

	macd := MACD(values, 12, 26, 9)
	if len(macd.DIF) != 50 || len(macd.DEA) != 50 || len(macd.Histogram) != 50 {
		t.Error("MACD 输出长度不匹配")
	}
}
