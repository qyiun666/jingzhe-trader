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

// GetByKeyword 按关键词模糊搜索新闻标题和内容
func (r *NewsRepo) GetByKeyword(keyword string) ([]model.News, error) {
	query := fmt.Sprintf(`SELECT %s FROM news
		WHERE title LIKE ? OR content LIKE ?
		ORDER BY datetime DESC`, newsSelectCols)
	var newsList []model.News
	like := "%" + strings.TrimSpace(keyword) + "%"
	if err := selectList(r.db, query, &newsList, "搜索新闻失败", like, like); err != nil {
		return nil, err
	}
	return newsList, nil
}
