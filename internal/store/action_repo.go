package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"jingzhe-trader/internal/model"
)

// ActionLog 统一动作日志 (kind: task/api/trade; 每动作一条, 可按日汇总)
type ActionLog struct {
	ID         int64  `db:"id" json:"id"`
	TradeDate  string `db:"trade_date" json:"trade_date"`
	Kind       string `db:"kind" json:"kind"`       // task=调度任务 / api=接口调用 / trade=人工成交回报
	Name       string `db:"name" json:"name"`       // 任务名或接口名
	Status     string `db:"status" json:"status"`   // success / fail
	Summary    string `db:"summary" json:"summary"` // 结果一句话
	Detail     string `db:"detail" json:"detail"`   // JSON: 参数/报错
	DurationMs int64  `db:"duration_ms" json:"duration_ms"`
	CreatedAt  string `db:"created_at" json:"created_at"`
}

// ActionRepo 动作日志仓储
type ActionRepo struct {
	db *sqlx.DB
}

// NewActionRepo 构造 ActionRepo
func NewActionRepo(db *sqlx.DB) *ActionRepo {
	return &ActionRepo{db: db}
}

// Insert 记录一次动作 (trade_date/created_at 缺省自动补)
func (r *ActionRepo) Insert(a ActionLog) error {
	if a.TradeDate == "" {
		a.TradeDate = time.Now().Format("20060102")
	}
	if a.CreatedAt == "" {
		a.CreatedAt = time.Now().Format(TimeLayout)
	}
	_, err := r.db.Exec(`INSERT INTO action_log
		(trade_date, kind, name, status, summary, detail, duration_ms, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		a.TradeDate, a.Kind, a.Name, a.Status, a.Summary, a.Detail, a.DurationMs, a.CreatedAt)
	if err != nil {
		return fmt.Errorf("记录动作日志失败: %w", err)
	}
	return nil
}

// ListByDate 按交易日查询动作日志 (升序)
func (r *ActionRepo) ListByDate(date string) ([]ActionLog, error) {
	var logs []ActionLog
	if err := r.db.Select(&logs,
		`SELECT id, trade_date, kind, name, status, summary, detail, duration_ms, created_at
		 FROM action_log WHERE trade_date = ? ORDER BY id`, date); err != nil {
		return nil, fmt.Errorf("查询动作日志失败: %w", err)
	}
	return logs, nil
}

// ListLatest 查询最近的 n 条动作日志
func (r *ActionRepo) ListLatest(n int) ([]ActionLog, error) {
	var logs []ActionLog
	if err := r.db.Select(&logs,
		`SELECT id, trade_date, kind, name, status, summary, detail, duration_ms, created_at
		 FROM action_log ORDER BY id DESC LIMIT ?`, n); err != nil {
		return nil, fmt.Errorf("查询动作日志失败: %w", err)
	}
	return logs, nil
}

// TradeFill 人工成交流水 (action_log.kind=trade 的 detail 载荷; 供对账还原)
type TradeFill struct {
	TsCode     string  `json:"ts_code"`
	Side       string  `json:"side"` // buy / sell
	Qty        int     `json:"qty"`
	Price      float64 `json:"price"`
	Amount     float64 `json:"amount"`
	Cash       float64 `json:"cash"`        // 成交后可用资金
	TotalAsset float64 `json:"total_asset"` // 成交后总资产
}

// AddTrade 记录一笔人工成交到动作日志 (kind=trade)
func (r *ActionRepo) AddTrade(tradeDate string, f TradeFill) error {
	detail, _ := json.Marshal(f)
	return r.Insert(ActionLog{
		TradeDate: tradeDate,
		Kind:      "trade",
		Name:      f.TsCode,
		Status:    "success",
		Summary:   fmt.Sprintf("%s %d股 @ %.2f", f.Side, f.Qty, f.Price),
		Detail:    string(detail),
	})
}

// ListTrades 按交易日返回人工成交流水 (kind=trade), 还原为 model.Trade 供对账使用
func (r *ActionRepo) ListTrades(date string) ([]model.Trade, error) {
	var logs []ActionLog
	if err := r.db.Select(&logs,
		`SELECT id, trade_date, kind, name, status, summary, detail, duration_ms, created_at
		 FROM action_log WHERE trade_date = ? AND kind = 'trade' ORDER BY id`, date); err != nil {
		return nil, fmt.Errorf("查询当日成交流水失败: %w", err)
	}
	var trades []model.Trade
	for _, l := range logs {
		var f TradeFill
		if err := json.Unmarshal([]byte(l.Detail), &f); err != nil || f.TsCode == "" {
			continue // detail 结构不符, 跳过
		}
		side := model.SideBuy
		if f.Side == "sell" {
			side = model.SideSell
		}
		trades = append(trades, model.Trade{
			TsCode:    f.TsCode,
			Side:      side,
			Price:     f.Price,
			Qty:       f.Qty,
			Amount:    f.Amount,
			TradeDate: l.TradeDate,
		})
	}
	return trades, nil
}
