// Package backtest 最小回测与 IC 度量：回答"这套因子到底有没有选股能力"。
//
// 设计立场（对照外部实践）：Microsoft Qlib 的 SignalAnalysis 把这件事标准化成
// RankIC / ICIR 序列；本包只做其中最小可用的一环，不重建整个回测引擎。
//
// 三条硬约束：
//  1. **复用生产因子代码**：因子值一律经 screener.BuildFactorScores / ComputeRaw 计算，
//     不另写一套。否则测出来的是"回测里的因子"，不是选股器真正用的那个。
//  2. **无前视偏差**：T 日截面只能用 ≤T 的日线（前复权）与 T 日估值，收益从 T 之后算。
//  3. **深历史不进 SQLite**：库里的 daily_bar 只留 45 天热窗口（操作数据），
//     回测要的多年历史落在库外的 CSV.gz（见 history.go）。这既避免"每天删历史"
//     与"回测要历史"的自相矛盾，也不让 NAS 上的主库被回测数据撑大。
//
// 依赖方向：backtest 依赖 screener（复用因子）、model、tushare（回补）；
// 交易日历由调用方注入，本包不直接依赖 store。
package backtest

import (
	"compress/gzip"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"jingzhe-trader/internal/model"
)

// PricePoint 一根日线（前复权收盘 + 成交量 + 未复权收盘）。
type PricePoint struct {
	Close    float64
	VolLot   float64
	RawClose float64
}

// ValPoint 一天的估值截面（选股漏斗的流动性/估值输入）。
type ValPoint struct {
	TurnoverRate float64
	PETtm        float64
	PB           float64
	CircMvW      float64
}

// History 回测历史：按日期为第一维索引（IC 是截面计算，天然按日期访问）。
//
//	Closes["20250630"]["600000.SH"] = 12.34
//
// 日期为第一维而非代码：动量/低波需要某只票在窗口内连续 21 天的收盘，按日期取
// 这批日期再按代码归拢，一次遍历即可，不必为每只票维护一条长切片。
type History struct {
	Dates     []string // 升序交易日
	Closes    map[string]map[string]float64
	Vols      map[string]map[string]float64
	Raws      map[string]map[string]float64
	Vals      map[string]map[string]ValPoint
	codesOnce []string
	dateSet   map[string]bool // MarkDate 去重用的集合（不导出，Finalize 不用它）
}

// NewHistory 构造空历史。
func NewHistory() *History {
	return &History{
		Closes: map[string]map[string]float64{},
		Vols:   map[string]map[string]float64{},
		Raws:   map[string]map[string]float64{},
		Vals:   map[string]map[string]ValPoint{},
	}
}

// AddBar 登记一根日线。
func (h *History) AddBar(tsCode, date string, p PricePoint) {
	if tsCode == "" || date == "" || p.Close <= 0 {
		return
	}
	put(h.Closes, date, tsCode, p.Close)
	put(h.Vols, date, tsCode, p.VolLot)
	put(h.Raws, date, tsCode, p.RawClose)
}

// AddValuation 登记一天的估值。
func (h *History) AddValuation(tsCode, date string, v ValPoint) {
	if tsCode == "" || date == "" {
		return
	}
	m := h.Vals[date]
	if m == nil {
		m = map[string]ValPoint{}
		h.Vals[date] = m
	}
	m[tsCode] = v
}

// MarkDate 把一个交易日登记进 Dates（去重、排序由 Finalize 完成）。
// 用集合做去重：Load 逐行调用，线性扫描会让耗时随"行数 × 日期数"增长。
func (h *History) MarkDate(date string) {
	if date == "" {
		return
	}
	if h.dateSet == nil {
		h.dateSet = make(map[string]bool, 1024)
	}
	if h.dateSet[date] {
		return
	}
	h.dateSet[date] = true
	h.Dates = append(h.Dates, date)
}

// Finalize 排序并重建派生索引（加载或回补结束后调用；可重复调用，幂等）。
func (h *History) Finalize() {
	sort.Strings(h.Dates)
	// codesOnce 每次整段重建而非增量追加：本函数在 Load→RunIC、Run→Save 等路径上会被
	// 调用不止一次，增量追加会让代码数按调用次数翻倍。
	seen := make(map[string]bool, len(h.codesOnce))
	h.codesOnce = h.codesOnce[:0]
	for date := range h.Closes {
		for code := range h.Closes[date] {
			if !seen[code] {
				seen[code] = true
				h.codesOnce = append(h.codesOnce, code)
			}
		}
	}
	sort.Strings(h.codesOnce)
}

// Codes 全部出现过的标的（升序）。
func (h *History) Codes() []string { return h.codesOnce }

// DateIndex 交易日 → 序号（供位移取"第 h 个交易日"）。
func (h *History) DateIndex() map[string]int {
	idx := make(map[string]int, len(h.Dates))
	for i, d := range h.Dates {
		idx[d] = i
	}
	return idx
}

// WindowCloses 取某标的在给定日期序列上的收盘（缺失日跳过），升序。
func (h *History) WindowCloses(code string, dates []string) []float64 {
	out := make([]float64, 0, len(dates))
	for _, d := range dates {
		if v, ok := h.Closes[d][code]; ok && v > 0 {
			out = append(out, v)
		}
	}
	return out
}

// put 写进"日期 → 代码 → 值"的两级 map。
func put(m map[string]map[string]float64, date, code string, v float64) {
	inner := m[date]
	if inner == nil {
		inner = map[string]float64{}
		m[date] = inner
	}
	inner[code] = v
}

// ===================== CSV.gz 编解码（深历史落库外）=====================

// barsFile / valsFile 两个历史文件的固定名。
const (
	barsFile = "bars.csv.gz"
	valsFile = "vals.csv.gz"
)

// Save 把历史写成两个 CSV.gz（bars / vals），返回写入的文件路径。
// 覆盖写：回补是幂等的整段重建，不做增量合并（增量合并会让缺口无法察觉）。
func (h *History) Save(dir string) ([]string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建历史目录 %s 失败: %w", dir, err)
	}
	h.Finalize()

	barsPath := filepath.Join(dir, barsFile)
	if err := writeCSVGz(barsPath, []string{"ts_code", "trade_date", "close", "vol_lot", "raw_close"}, func(w *csv.Writer) error {
		for _, date := range h.Dates {
			inner := h.Closes[date]
			codes := sortedKeys(inner)
			for _, code := range codes {
				row := []string{code, date,
					ftoa(inner[code]), ftoa(h.Vols[date][code]), ftoa(h.Raws[date][code])}
				if err := w.Write(row); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}

	valsPath := filepath.Join(dir, valsFile)
	if err := writeCSVGz(valsPath, []string{"ts_code", "trade_date", "turnover_rate", "pe_ttm", "pb", "circ_mv_w"}, func(w *csv.Writer) error {
		for _, date := range h.Dates {
			inner := h.Vals[date]
			codes := sortedKeysVal(inner)
			for _, code := range codes {
				v := inner[code]
				row := []string{code, date, ftoa(v.TurnoverRate), ftoa(v.PETtm), ftoa(v.PB), ftoa(v.CircMvW)}
				if err := w.Write(row); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return []string{barsPath, valsPath}, nil
}

// Load 读取历史目录下的两个 CSV.gz；文件不存在时返回空历史与 found=false。
func Load(dir string) (*History, bool, error) {
	h := NewHistory()
	barsPath := filepath.Join(dir, barsFile)
	valsPath := filepath.Join(dir, valsFile)
	if !fileExists(barsPath) {
		return h, false, nil
	}
	err := readCSVGz(barsPath, func(rec []string) error {
		if len(rec) < 5 {
			return fmt.Errorf("bars 行列数不足: %v", rec)
		}
		code, date := rec[0], rec[1]
		closeV, err := atof(rec[2])
		if err != nil {
			return fmt.Errorf("bars 行 %s/%s close: %w", code, date, err)
		}
		vol, err := atof(rec[3])
		if err != nil {
			return fmt.Errorf("bars 行 %s/%s vol_lot: %w", code, date, err)
		}
		raw, err := atof(rec[4])
		if err != nil {
			return fmt.Errorf("bars 行 %s/%s raw_close: %w", code, date, err)
		}
		h.AddBar(code, date, PricePoint{Close: closeV, VolLot: vol, RawClose: raw})
		h.MarkDate(date)
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if fileExists(valsPath) {
		if err := readCSVGz(valsPath, func(rec []string) error {
			if len(rec) < 6 {
				return nil // 旧的/残缺估值行跳过，不阻断 bars
			}
			vals := make([]float64, 4)
			for i, raw := range rec[2:6] {
				v, err := atof(raw)
				if err != nil {
					return fmt.Errorf("vals 行 %s/%s 第 %d 列: %w", rec[0], rec[1], i+2, err)
				}
				vals[i] = v
			}
			h.AddValuation(rec[0], rec[1], ValPoint{
				TurnoverRate: vals[0], PETtm: vals[1], PB: vals[2], CircMvW: vals[3],
			})
			return nil
		}); err != nil {
			return nil, false, err
		}
	}
	h.Finalize()
	return h, true, nil
}

// writeCSVGz 建文件并逐行写（gzip 压缩），fn 负责产出全部行。
func writeCSVGz(path string, header []string, fn func(*csv.Writer) error) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("创建 %s 失败: %w", path, err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	cw := csv.NewWriter(gz)
	if err := cw.Write(header); err != nil {
		return fmt.Errorf("写表头失败: %w", err)
	}
	if err := fn(cw); err != nil {
		return fmt.Errorf("写 %s 失败: %w", path, err)
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return fmt.Errorf("刷新 CSV 失败: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("关闭 gzip 失败: %w", err)
	}
	return f.Sync()
}

// readCSVGz 逐行读（跳过表头），rec 已保证是数据行。
func readCSVGz(path string, fn func(rec []string) error) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("打开 %s 失败: %w", path, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("解压 %s 失败: %w", path, err)
	}
	defer gz.Close()
	cr := csv.NewReader(gz)
	cr.FieldsPerRecord = -1 // 容忍残缺行（由各调用方自行判列数）
	first := true
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("读 %s 失败: %w", path, err)
		}
		if first {
			first = false
			continue // 表头
		}
		if err := fn(rec); err != nil {
			return err
		}
	}
	return nil
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi != nil && !fi.IsDir()
}

// ftoa 定点字符串：4 位小数。
//
// 有损但可接受 —— 前复权价不在 0.01 网格上，截断误差 ≤5e-5，对截面秩相关（只依赖排序）
// 无影响。A 股最小价位 0.01 元，未复权价在 4 位下是精确的。
func ftoa(v float64) string {
	if v == 0 {
		return "0"
	}
	return strconv.FormatFloat(v, 'f', 4, 64)
}

// atof 解析数值列；失败返回 error 而不是 0。
//
// 静默返回 0 会让坏行的 close 变成 0、被 AddBar 丢掉，于是"某些截面收益算不出来"
// 这种缺失永远无法归因。研究数据集宁可加载时就报错，也不要一个悄悄缺了几行的历史。
func atof(s string) (float64, error) {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf("数值列 %q 无法解析: %w", s, err)
	}
	return v, nil
}

func sortedKeys(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeysVal(m map[string]ValPoint) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Bar 把一根历史日线转成 model.Bar（供复用 store/选股的读取口径）。
func (h *History) Bar(code, date string) model.Bar {
	return model.Bar{
		TsCode: code, TradeDate: date,
		Close:    model.FromFloat(h.Closes[date][code]),
		VolLot:   h.Vols[date][code],
		RawClose: model.FromFloat(h.Raws[date][code]),
	}
}
