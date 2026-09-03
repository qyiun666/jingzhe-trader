package dataloader

import (
	"context"
	"sort"
	"time"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
)

// FinaProvider 财务慢路径数据提供接口（便于单测注入假实现，无需真实触网）。
type FinaProvider interface {
	StockBasic(ctx context.Context) ([]model.StockBasic, error)
	FinaIndicator(ctx context.Context, tsCode string) ([]model.FinaIndicator, error)
}

// fina 状态常量（与 fina_sync_state.status 列一致）。
const (
	finaStatusRunning     = "running"
	finaStatusSuccess     = "success"
	finaStatusInterrupted = "interrupted"
	// cursorDone 游标哨兵：表示全量已完成（区别于空串"首次运行"）。
	cursorDone = "__DONE__"
)

// FinaSyncer 财务慢路径同步器。
//
// 关键设计（docs/tech-constraints.md §2）：
//   - 逐只 ts_code 调用 fina_indicator（无全市场批量入口）；
//   - 限流由 tushare.Client 内部令牌桶统一控制；瞬时错误由 Call 内部指数退避；
//   - 通过 fina_sync_state.cursor_ts_code + fina_sync_item 实现【进程中断可续传】：
//     游标指向下一只待处理股票；kill 后重启从游标继续，已处理的不重做（幂等 upsert）。
type FinaSyncer struct {
	store   *store.Store
	provider FinaProvider
	now     func() time.Time
}

// NewFinaSyncer 构造财务同步器。
func NewFinaSyncer(s *store.Store, p FinaProvider) *FinaSyncer {
	return &FinaSyncer{store: s, provider: p, now: time.Now}
}

// Run 执行财务同步。limit>0 时本批次最多处理 limit 只股票（用于分批/CLI 测试）。
// 通过 fina_sync_state 持久化游标，进程中断（ctx 取消）后重启可续传。
func (s *FinaSyncer) Run(ctx context.Context, limit int) error {
	fr := s.store.FinaRepo()
	st, err := fr.GetSyncState(ctx)
	if err != nil {
		// 首次运行：初始化状态行（id=1）
		st = store.FinaSyncState{ID: 1, Status: finaStatusRunning, StartedAt: s.now().UTC().Format(time.RFC3339)}
	}

	// 拉取全市场股票列表（有序，保证续传顺序稳定）
	basics, err := s.provider.StockBasic(ctx)
	if err != nil {
		return err
	}
	codes := make([]string, 0, len(basics))
	for i := range basics {
		basics[i].UpdatedAt = s.now().UTC().Format(time.RFC3339)
		// 持久化股票基础信息：候选池/持仓覆盖检查（新鲜度门禁 #7）依赖 stock_basic。
		if uerr := s.store.MarketRepo().UpsertStockBasic(ctx, basics[i]); uerr != nil {
			return uerr
		}
		codes = append(codes, basics[i].TsCode)
	}
	sort.Strings(codes)

	st.Total = len(codes)
	st.Status = finaStatusRunning
	if st.StartedAt == "" {
		st.StartedAt = s.now().UTC().Format(time.RFC3339)
	}

	// 从游标恢复：
	//   cursorDone 哨兵 → 已全量完成，跳过；
	//   空串        → 首次运行，从头开始；
	//   命中某代码  → 从该代码继续（断点续传）；
	//   未命中      → 从头开始（游标失效，幂等重做）。
	startIdx := 0
	if st.CursorTsCode == cursorDone {
		startIdx = len(codes)
	} else if st.CursorTsCode != "" {
		if idx := sort.SearchStrings(codes, st.CursorTsCode); idx < len(codes) && codes[idx] == st.CursorTsCode {
			startIdx = idx
		}
	}

	done := st.Done
	failed := st.Failed
	for i := startIdx; i < len(codes); i++ {
		// 进程中断点：ctx 取消时记录下一只待处理股票并退出（可续传）
		select {
		case <-ctx.Done():
			st.CursorTsCode = codes[i]
			st.Status = finaStatusInterrupted
			st.FinishedAt = s.now().UTC().Format(time.RFC3339)
			// 注意：ctx 已取消，必须用独立 context 持久化游标，否则写库会因 ctx.Err 失败。
			_ = fr.UpsertSyncState(context.Background(), st)
			return ctx.Err()
		default:
		}

		tsCode := codes[i]
		indicators, ferr := s.provider.FinaIndicator(ctx, tsCode)
		if ferr != nil {
			// 单只失败不阻断整体：记录失败项，继续下一只
			failed++
			if aerr := fr.UpsertSyncItem(context.Background(), tsCode, "failed", ferr.Error(), s.now().UTC().Format(time.RFC3339), 1, ""); aerr != nil {
				return aerr
			}
			st.CursorTsCode = tsCode
			st.Failed = failed
			if i%50 == 0 {
				_ = fr.UpsertSyncState(context.Background(), st)
			}
			continue
		}
		for _, f := range indicators {
			// 幂等 upsert，使用独立 context 避免 ctx 取消时写库失败导致提前返回（丢失中断态）。
			if uerr := fr.UpsertFinaIndicator(context.Background(), f); uerr != nil {
				return uerr
			}
		}
		done++
		lastEnd := ""
		if len(indicators) > 0 {
			lastEnd = indicators[len(indicators)-1].EndDate
		}
		if aerr := fr.UpsertSyncItem(context.Background(), tsCode, "done", "", s.now().UTC().Format(time.RFC3339), 1, lastEnd); aerr != nil {
			return aerr
		}
		// 游标推进到下一只（已处理的不再重做）
		st.CursorTsCode = nextCode(codes, i)
		st.Done = done
		st.Failed = failed
		// 频繁持久化游标（每 20 只或达批次上限），保证断点续传精度。
		// 同上使用独立 context：ctx 取消时写库失败应转为顶部中断态，而非提前以 running 退出。
		if i%20 == 0 || (limit > 0 && done >= limit) {
			_ = fr.UpsertSyncState(context.Background(), st)
		}
		if limit > 0 && done >= limit {
			break
		}
	}

	if st.Status != finaStatusInterrupted {
		if done >= st.Total {
			st.Status = finaStatusSuccess
			st.CursorTsCode = cursorDone
		}
		// 受 limit 限制未跑完：保持 running，游标已指向下一只待处理
		st.FinishedAt = s.now().UTC().Format(time.RFC3339)
	}
	if perr := fr.UpsertSyncState(ctx, st); perr != nil {
		return perr
	}
	return nil
}

// nextCode 返回 codes[i] 之后一只股票的 ts_code（已全量则返回空串）。
func nextCode(codes []string, i int) string {
	if i+1 < len(codes) {
		return codes[i+1]
	}
	return ""
}

// SyncFina 便捷入口：以 tushare 客户端为数据源执行财务同步。
func (d *Dataloader) SyncFina(ctx context.Context, limit int) error {
	return NewFinaSyncer(d.store, d.tushare).Run(ctx, limit)
}
