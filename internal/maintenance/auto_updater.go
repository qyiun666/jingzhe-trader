package maintenance

import (
	"fmt"
	"os/exec"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/pkg/logger"
)

// Status 系统状态
type Status struct {
	Healthy         bool   `json:"healthy"`
	LastDataUpdate  string `json:"last_data_update"`
	LastDataDate    string `json:"last_data_date"`    // 数据库中最新的行情日期
	Today           string `json:"today"`
	DataFresh       bool   `json:"data_fresh"`        // 数据是否是最新的
	Uptime          string `json:"uptime"`
	CurrentStrategy string `json:"current_strategy"`
	PortfolioCount  int    `json:"portfolio_count"`   // 持仓数量
	NextMarketOpen  string `json:"next_market_open"`  // 下一个交易日
}

// AutoUpdater 自动维护器
type AutoUpdater struct {
	cfg        *config.Config
	db         *sqlx.DB
	calRepo    *store.CalendarRepo
	barRepo    *store.BarRepo
	portRepo   *store.PortfolioRepo
	mu         sync.RWMutex
	startTime  time.Time
	lastUpdate time.Time
	binPath    string // dataloader 二进制路径
	configPath string // 配置文件路径
}

// NewAutoUpdater 创建自动维护器
func NewAutoUpdater(cfg *config.Config, db *sqlx.DB) *AutoUpdater {
	return &AutoUpdater{
		cfg:        cfg,
		db:         db,
		calRepo:    store.NewCalendarRepo(db),
		barRepo:    store.NewBarRepo(db),
		portRepo:   store.NewPortfolioRepo(db),
		startTime:  time.Now(),
		binPath:    "bin/dataloader",
		configPath: config.DefaultConfigPath(),
	}
}

// SetBinPath 设置 dataloader 二进制路径（可选，默认 "bin/dataloader"）
func (au *AutoUpdater) SetBinPath(path string) {
	au.mu.Lock()
	defer au.mu.Unlock()
	au.binPath = path
}

// SetConfigPath 设置配置文件路径（可选，默认使用 config.DefaultConfigPath()）
func (au *AutoUpdater) SetConfigPath(path string) {
	au.mu.Lock()
	defer au.mu.Unlock()
	au.configPath = path
}

// RunHealthCheck 执行健康检查
func (au *AutoUpdater) RunHealthCheck() Status {
	au.mu.RLock()
	defer au.mu.RUnlock()

	var status Status
	status.Healthy = true
	status.Today = time.Now().Format("20060102")

	// 1. 检查数据库连接（Ping）
	if err := au.db.Ping(); err != nil {
		status.Healthy = false
		logger.L().Errorw("健康检查: 数据库连接失败", "err", err)
	}

	// 2. 查询最新行情日期
	if status.Healthy {
		maxDate, err := au.barRepo.GetMaxTradeDate()
		if err != nil {
			status.Healthy = false
			logger.L().Errorw("健康检查: 查询最新行情日期失败", "err", err)
		} else {
			status.LastDataDate = maxDate
		}
	}

	// 3. 判断数据是否新鲜：最新数据日期 == 最近交易日
	if status.Healthy && status.LastDataDate != "" {
		// 获取今天之前最近的交易日
		preTradeDate, err := au.calRepo.GetPreTradeDate(status.Today)
		if err != nil {
			logger.L().Warnw("健康检查: 查询上一交易日失败", "err", err)
		} else if preTradeDate != "" {
			status.DataFresh = (status.LastDataDate == preTradeDate)
		}
	}

	// 4. 查询持仓数量
	if status.Healthy {
		positions, err := au.portRepo.GetAllPositions()
		if err != nil {
			logger.L().Warnw("健康检查: 查询持仓数量失败", "err", err)
		} else {
			status.PortfolioCount = len(positions)
		}
	}

	// 5. 查询下一个交易日
	if status.Healthy {
		nextDate, err := au.calRepo.GetNextTradeDate(status.Today)
		if err != nil {
			logger.L().Warnw("健康检查: 查询下一交易日失败", "err", err)
		} else {
			status.NextMarketOpen = nextDate
		}
	}

	// 6. 记录上次更新时间
	au.mu.RLock()
	if !au.lastUpdate.IsZero() {
		status.LastDataUpdate = au.lastUpdate.Format("2006-01-02 15:04:05")
	}
	au.mu.RUnlock()

	// 7. 计算运行时间
	status.Uptime = time.Since(au.startTime).Truncate(time.Second).String()

	return status
}

// UpdateData 执行数据更新
// 调用 bin/dataloader -config config.yaml 来拉取最新行情
func (au *AutoUpdater) UpdateData() error {
	au.mu.Lock()
	defer au.mu.Unlock()

	logger.L().Infow("开始执行数据更新", "bin", au.binPath, "config", au.configPath)

	// 构造命令: bin/dataloader -config config.yaml
	cmd := exec.Command(au.binPath, "-config", au.configPath)

	// 捕获标准输出和标准错误
	output, err := cmd.CombinedOutput()
	if err != nil {
		logger.L().Errorw("数据更新执行失败",
			"err", err,
			"output", string(output),
		)
		return fmt.Errorf("执行 dataloader 失败: %w, output: %s", err, string(output))
	}

	// 记录更新时间
	au.lastUpdate = time.Now()

	logger.L().Infow("数据更新完成", "output", string(output))
	return nil
}

// ShouldUpdate 判断是否需要更新数据
// 条件: 最新数据日期 < 今天 && 今天是交易日 && 当前时间 > 16:00（收盘后）
func (au *AutoUpdater) ShouldUpdate() bool {
	au.mu.RLock()
	defer au.mu.RUnlock()

	today := time.Now().Format("20060102")

	// 查询最新行情日期
	maxDate, err := au.barRepo.GetMaxTradeDate()
	if err != nil {
		logger.L().Warnw("ShouldUpdate: 查询最新行情日期失败", "err", err)
		return false
	}

	// 如果数据日期 >= 今天，不需要更新
	if maxDate >= today {
		return false
	}

	// 判断今天是否为交易日
	isTradeDay, err := au.calRepo.IsTradeDay(today)
	if err != nil {
		logger.L().Warnw("ShouldUpdate: 判断交易日失败", "err", err)
		return false
	}
	if !isTradeDay {
		return false
	}

	// 判断当前时间是否在 16:00 之后（收盘后才能拉到当日数据）
	now := time.Now()
	updateThreshold := time.Date(now.Year(), now.Month(), now.Day(), 16, 0, 0, 0, now.Location())
	if now.Before(updateThreshold) {
		return false
	}

	return true
}