package store

import (
	"context"
	"database/sql"
	"errors"
)

// 元数据键名
const (
	MetaExifToolVersion = "exiftool_version"
)

// GetMeta 读取 schema_meta 里的键值，不存在返回空串。
func (s *Store) GetMeta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM schema_meta WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return v, nil
}

// SetMeta 写入 schema_meta 键值。
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO schema_meta(key, value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// ResetNoCIDAssets 把「上一轮没读到 ContentIdentifier」的资产标记为待重新解析。
//
// 用途：exiftool 升级后，旧版本读不出的 HEIC 需要重新解析一次
// （典型场景：Debian 自带 12.57 读不了 iOS 17+ 新 HEIC 的 Apple 元数据）。
// 置为 unreadable 是为了让扫描器的「上次读取失败则重试」分支命中。
func (s *Store) ResetNoCIDAssets(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE live_assets SET
		content_id=NULL, cid_source=NULL, state=?, warning=?, updated_at=?
		WHERE state=? AND (content_id IS NULL OR content_id='')`,
		AssetStateUnreadable, "元数据解析工具已升级，等待重新解析", Now(), AssetStateNoCID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
