package store

import (
	"context"
	"database/sql"
	"fmt"
)

// AssetUpsertResult 批量登记的返回结果。
type AssetUpsertResult struct {
	Asset   *Asset
	Created bool
	Prev    Asset
}

// UpsertAssets 在一个事务里批量登记源文件，减少 fsync 次数。
func (s *Store) UpsertAssets(ctx context.Context, assets []*Asset) ([]AssetUpsertResult, error) {
	out := make([]AssetUpsertResult, 0, len(assets))
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		for _, a := range assets {
			created, prev, err := upsertAsset(ctx, tx, a)
			if err != nil {
				return err
			}
			out = append(out, AssetUpsertResult{Asset: a, Created: created, Prev: prev})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ContentIDUpdate 单个资产的 ContentIdentifier 更新。
type ContentIDUpdate struct {
	ID        int64
	ContentID string
	Source    string
	State     string // 非空时一并更新状态
	Warning   string
}

// BulkSetContentID 批量写入 ContentIdentifier。
func (s *Store) BulkSetContentID(ctx context.Context, updates []ContentIDUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	now := Now()
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		for _, u := range updates {
			var err error
			if u.State != "" {
				_, err = tx.ExecContext(ctx, `UPDATE live_assets SET
					content_id=NULLIF(?,''), cid_source=NULLIF(?,''), state=?, warning=NULLIF(?,''), updated_at=?
					WHERE id=?`, u.ContentID, u.Source, u.State, u.Warning, now, u.ID)
			} else {
				_, err = tx.ExecContext(ctx, `UPDATE live_assets SET
					content_id=NULLIF(?,''), cid_source=NULLIF(?,''), updated_at=? WHERE id=?`,
					u.ContentID, u.Source, now, u.ID)
			}
			if err != nil {
				return fmt.Errorf("写入 ContentIdentifier 失败: %w", err)
			}
		}
		return nil
	})
}

// MD5Update 单个资产的 MD5 更新。
type MD5Update struct {
	ID        int64
	MD5       string
	MtimeUnix int64
}

// BulkSetMD5 批量写入 MD5。
func (s *Store) BulkSetMD5(ctx context.Context, updates []MD5Update) error {
	if len(updates) == 0 {
		return nil
	}
	now := Now()
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		for _, u := range updates {
			if _, err := tx.ExecContext(ctx, `UPDATE live_assets SET
				md5=?, md5_mtime_unix=?, md5_checked_at=?, updated_at=? WHERE id=?`,
				u.MD5, u.MtimeUnix, now, now, u.ID); err != nil {
				return fmt.Errorf("写入 MD5 失败: %w", err)
			}
		}
		return nil
	})
}

// StateUpdate 单个资产的状态更新。
type StateUpdate struct {
	ID      int64
	State   string
	Warning string
}

// BulkSetState 批量更新状态。
func (s *Store) BulkSetState(ctx context.Context, updates []StateUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	now := Now()
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		for _, u := range updates {
			if _, err := tx.ExecContext(ctx, `UPDATE live_assets SET
				state=?, warning=NULLIF(?,''), updated_at=? WHERE id=?`,
				u.State, u.Warning, now, u.ID); err != nil {
				return fmt.Errorf("更新资产状态失败: %w", err)
			}
		}
		return nil
	})
}
