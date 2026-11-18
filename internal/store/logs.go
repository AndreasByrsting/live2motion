package store

import (
	"context"
	"errors"
	"strconv"
)

// 日志级别
const (
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// LogEntry 一条运行日志。
type LogEntry struct {
	ID      int64  `json:"id"`
	TS      int64  `json:"ts"`
	Level   string `json:"level"`
	Scope   string `json:"scope"`
	Message string `json:"message"`
}

// AppendLog 持久化一条日志。
func (s *Store) AppendLog(ctx context.Context, level, scope, message string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO logs(ts, level, scope, message) VALUES(?,?,?,?)`,
		Now(), level, scope, message)
	return err
}

// ListLogs 按级别过滤返回最近日志（按 id 升序，便于前端追加）。
func (s *Store) ListLogs(ctx context.Context, level string, limit int) ([]LogEntry, error) {
	if limit <= 0 || limit > 2000 {
		limit = 300
	}
	q := `SELECT id, ts, level, scope, message FROM logs`
	args := []any{}
	switch level {
	case "", "all":
		// 不过滤
	case "warn":
		// 与前端一致：选 WARN 时 ERROR 也显示（错误比警告严重，过滤掉反而漏信息）
		q += ` WHERE level IN ('warn','error')`
	default:
		q += ` WHERE level=?`
		args = append(args, level)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rev []LogEntry
	for rows.Next() {
		var e LogEntry
		if err := rows.Scan(&e.ID, &e.TS, &e.Level, &e.Scope, &e.Message); err != nil {
			return nil, err
		}
		rev = append(rev, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 反转为时间正序
	out := make([]LogEntry, len(rev))
	for i, e := range rev {
		out[len(rev)-1-i] = e
	}
	return out, nil
}

// PruneLogs 只保留最近 keep 条日志，防止数据库无限增长。
func (s *Store) PruneLogs(ctx context.Context, keep int) error {
	if keep < 100 {
		keep = 100
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM logs WHERE id <= (SELECT COALESCE(MAX(id),0) - ? FROM logs)`, keep)
	return err
}

// ClearLogs 清空日志。
func (s *Store) ClearLogs(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM logs`)
	return err
}

// LogLevelRank 用于判定是否输出（info=1, warn=2, error=3）。
func LogLevelRank(level string) int {
	switch level {
	case LevelError:
		return 3
	case LevelWarn:
		return 2
	default:
		return 1
	}
}

// ParseInt 容错解析整数（设置表单提交的空串等）。
func ParseInt(raw string, def int) int {
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return v
}

var errNoSettings = errors.New("settings not found")
