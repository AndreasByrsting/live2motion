package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动，无需 CGO
)

// Store 封装 SQLite 访问。所有写入都走事务，保证崩溃后状态可恢复。
type Store struct {
	db *sql.DB
}

// Open 打开（必要时创建）数据库并执行迁移。
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("创建数据库目录失败: %w", err)
	}
	dsn := buildDSN(path)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	// WAL 模式下读写可并发，这里给足连接数避免 Web 查询与转换流程互相阻塞。
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func buildDSN(path string) string {
	// modernc.org/sqlite 通过 _pragma 查询参数在每条连接上应用 PRAGMA。
	p := filepath.ToSlash(path)
	p = strings.ReplaceAll(p, " ", "%20")
	return "file:" + p + "?_pragma=busy_timeout(10000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)"
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// DB 暴露底层连接，仅供同包与测试使用。
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("初始化表结构失败: %w", err)
	}
	var v int
	err := s.db.QueryRowContext(ctx, `SELECT CAST(value AS INTEGER) FROM schema_meta WHERE key='schema_version'`).Scan(&v)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO schema_meta(key, value) VALUES('schema_version', ?)`, fmt.Sprint(schemaVersion)); err != nil {
			return fmt.Errorf("写入 schema 版本失败: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("读取 schema 版本失败: %w", err)
	}
	if v > schemaVersion {
		return fmt.Errorf("数据库版本 %d 高于程序支持的 %d，请升级程序", v, schemaVersion)
	}
	// v < schemaVersion 时在此按版本追加 ALTER 迁移。
	if v != schemaVersion {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE schema_meta SET value=? WHERE key='schema_version'`, fmt.Sprint(schemaVersion)); err != nil {
			return fmt.Errorf("更新 schema 版本失败: %w", err)
		}
	}
	return nil
}

// WithTx 在事务中执行 fn，遇到 SQLITE_BUSY 自动重试。
func (s *Store) WithTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			lastErr = err
			if isBusy(err) {
				sleepCtx(ctx, time.Duration(50*(attempt+1))*time.Millisecond)
				continue
			}
			return err
		}
		if err := fn(tx); err != nil {
			_ = tx.Rollback()
			if isBusy(err) {
				lastErr = err
				sleepCtx(ctx, time.Duration(50*(attempt+1))*time.Millisecond)
				continue
			}
			return err
		}
		if err := tx.Commit(); err != nil {
			lastErr = err
			if isBusy(err) {
				sleepCtx(ctx, time.Duration(50*(attempt+1))*time.Millisecond)
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("事务重试耗尽: %w", lastErr)
}

func isBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") || strings.Contains(msg, "sqlite_busy")
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// Now 返回当前 Unix 秒。
func Now() int64 { return time.Now().Unix() }
