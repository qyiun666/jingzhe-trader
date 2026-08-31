package store

import (
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"

	"jingzhe-trader/internal/model"
)

// NewsRepo 新闻快讯仓储
type NewsRepo struct {
	db *sqlx.DB
}

// NewNewsRepo 构造 NewsRepo
func NewNewsRepo(db *sqlx.DB) *NewsRepo {
	return &NewsRepo{db: db}
}

const newsInsertSQL = `INSERT OR IGNORE INTO news
	(datetime, content, title, channels)
	VALUES (?, ?, ?, ?)`

const newsSelectCols = `id, datetime, content, title, channels`

// BatchInsert 批量插入新闻快讯(已存在则忽略, 避免重复)
func (r *NewsRepo) BatchInsert(newsList []model.News) error {
	return batchInsert(r.db, newsInsertSQL, "插入新闻失败", len(newsList), func(stmt *sqlx.Stmt, i int) error {
		n := newsList[i]
		_, err := stmt.Exec(n.Datetime, n.Content, n.Title, n.Channels)
		if err != nil {
			return fmt.Errorf("datetime=%s: %w", n.Datetime, err)
		}
		return nil
	})
}

// GetRecent 获取最近 n 条新闻(按时间倒序)
func (r *NewsRepo) GetRecent(n int) ([]model.News, error) {
	query := fmt.Sprintf(`SELECT %s FROM news ORDER BY datetime DESC LIMIT ?`, newsSelectCols)
	var newsList []model.News
	if err := selectList(r.db, query, &newsList, "查询新闻失败", n); err != nil {
		return nil, err
	}
	return newsList, nil
}

// GetMatching 检索 sinceDate 起 (含) 提到任一关键字的新闻, 按时间倒序最多 limit 条
//   - sinceDate 用 "2006-01-02" 前缀形式: news.datetime 存 "2006-01-02 15:04:05", 按字典序即可比较
//   - 匹配下沉到 SQL 检索整段保留窗口, 而不是"取最近 N 条再在内存里筛" —— 后者在全市场新闻流里
//     几乎不可能命中某只票, 个股新闻分析会长期只在"无相关新闻"里打转
//   - 用 instr 而非 LIKE: 股票名称可能含 "_" 等被 LIKE 当作通配符的字符
func (r *NewsRepo) GetMatching(sinceDate string, keywords []string, limit int) ([]model.News, error) {
	var conds []string
	args := make([]any, 0, len(keywords)*2+2)
	args = append(args, sinceDate)
	for _, kw := range keywords {
		if kw == "" {
			continue
		}
		conds = append(conds, "(instr(title, ?) > 0 OR instr(content, ?) > 0)")
		args = append(args, kw, kw)
	}
	if len(conds) == 0 {
		return nil, nil
	}
	query := fmt.Sprintf(`SELECT %s FROM news WHERE datetime >= ? AND (%s) ORDER BY datetime DESC LIMIT ?`,
		newsSelectCols, strings.Join(conds, " OR "))
	args = append(args, limit)
	var newsList []model.News
	if err := selectList(r.db, query, &newsList, "检索相关新闻失败", args...); err != nil {
		return nil, err
	}
	return newsList, nil
}
