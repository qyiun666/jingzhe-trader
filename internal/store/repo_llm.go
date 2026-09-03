package store

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"
	"jingzhe-trader/internal/model"
)

// LLMRepo LLM 域仓储：llm_call（Batch 4，T04）。
type LLMRepo struct {
	wdb *sqlx.DB
	rdb *sqlx.DB
}

// LLMRepo 返回 LLM 域仓储。
func (s *Store) LLMRepo() *LLMRepo {
	return &LLMRepo{wdb: s.writeDB, rdb: s.readDB}
}

// InsertCall 写入一条 LLM 终审记录。
func (r *LLMRepo) InsertCall(ctx context.Context, c model.LLMCall) error {
	const q = `INSERT INTO llm_call
		(trade_date, signal_id, ts_code, verdict, confidence, rationale, status, error, review_date, review_ret_pct, review_correct, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := r.wdb.ExecContext(ctx, q,
		c.TradeDate, c.SignalID, c.TsCode, c.Verdict, c.Confidence, c.Rationale,
		c.Status, c.Error, c.ReviewDate, c.ReviewRetPct, c.ReviewCorrect, c.CreatedAt,
	); err != nil {
		return fmt.Errorf("写入 LLM 调用记录 %s 失败: %w", c.TsCode, err)
	}
	return nil
}

// ListCalls 读取指定交易日的 LLM 终审记录。
func (r *LLMRepo) ListCalls(ctx context.Context, tradeDate string) ([]model.LLMCall, error) {
	var cs []model.LLMCall
	if err := r.rdb.SelectContext(ctx, &cs,
		`SELECT id, trade_date, COALESCE(signal_id,0) AS signal_id, ts_code, COALESCE(verdict,'') AS verdict,
		 COALESCE(confidence,0) AS confidence, COALESCE(rationale,'') AS rationale, status,
		 COALESCE(error,'') AS error, COALESCE(review_date,'') AS review_date,
		 COALESCE(review_ret_pct,0) AS review_ret_pct, COALESCE(review_correct,0) AS review_correct, created_at
		 FROM llm_call WHERE trade_date=? ORDER BY id`, tradeDate); err != nil {
		return nil, fmt.Errorf("读取 LLM 调用记录 %s 失败: %w", tradeDate, err)
	}
	return cs, nil
}
