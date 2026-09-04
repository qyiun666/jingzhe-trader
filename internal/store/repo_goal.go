package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"
	"jingzhe-trader/internal/model"
)

// goalStateKey 档位状态在 config_kv 里的唯一键。
//
// 它不配独立一张表：整个状态只有一行，且每次写都是整行覆盖（没有任何按列更新、
// 没有任何按列查询），列名从来不是查询条件。档位变更的历史只写日志，不入库。
const goalStateKey = "goal.state"

// GoalRepo 档位状态机读写：一个序列化的 model.GoalState 存在 config_kv 的一个键上。
type GoalRepo struct {
	wdb *sqlx.DB
	rdb *sqlx.DB
}

// GoalRepo 返回目标域仓储。
func (s *Store) GoalRepo() *GoalRepo {
	return &GoalRepo{wdb: s.writeDB, rdb: s.readDB}
}

// UpsertGoalState 覆盖当前档位状态。
func (r *GoalRepo) UpsertGoalState(ctx context.Context, g model.GoalState) error {
	raw, err := json.Marshal(g)
	if err != nil {
		return fmt.Errorf("序列化档位状态失败: %w", err)
	}
	cr := ConfigRepo{db: r.wdb}
	if err := cr.Set(ctx, goalStateKey, string(raw)); err != nil {
		return fmt.Errorf("写入档位状态失败: %w", err)
	}
	return nil
}

// GetGoalState 读取当前档位状态。
//
// 尚未初始化时返回包装了 sql.ErrNoRows 的错误 —— goal 的 loadState 据此建初始状态，
// 这个契约从表时代沿用下来，没有换成 bool 返回值。
func (r *GoalRepo) GetGoalState(ctx context.Context) (model.GoalState, error) {
	var v string
	err := r.rdb.GetContext(ctx, &v, "SELECT value FROM config_kv WHERE key = ?", goalStateKey)
	if errors.Is(err, sql.ErrNoRows) {
		return model.GoalState{}, fmt.Errorf("档位状态尚未初始化: %w", sql.ErrNoRows)
	}
	if err != nil {
		return model.GoalState{}, fmt.Errorf("读取档位状态失败: %w", err)
	}
	var g model.GoalState
	if err := json.Unmarshal([]byte(v), &g); err != nil {
		return model.GoalState{}, fmt.Errorf("解析档位状态失败（键 %s）: %w", goalStateKey, err)
	}
	return g, nil
}
