package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Pair 一组已确认的 Live Photo（HEIC + 视频），配对依据只有 ContentIdentifier。
type Pair struct {
	ID           int64
	SourceRoot   string
	ContentID    string
	HeicAssetID  int64
	VideoAssetID int64
	HeicRelPath  string
	VideoRelPath string
	HeicMtime    int64
	VideoMtime   int64
	HeicSize     int64
	VideoSize    int64
	CreatedAt    int64
	UpdatedAt    int64
}

const pairColumns = `id, source_root, content_id, heic_asset_id, video_asset_id,
	heic_rel_path, video_rel_path, heic_mtime, video_mtime, heic_size, video_size,
	created_at, updated_at`

func scanPair(row interface{ Scan(...any) error }) (Pair, error) {
	var p Pair
	err := row.Scan(&p.ID, &p.SourceRoot, &p.ContentID, &p.HeicAssetID, &p.VideoAssetID,
		&p.HeicRelPath, &p.VideoRelPath, &p.HeicMtime, &p.VideoMtime, &p.HeicSize, &p.VideoSize,
		&p.CreatedAt, &p.UpdatedAt)
	return p, err
}

// UpsertPair 登记配对，返回配对 id 以及「源文件指纹是否发生变化」。
// changed=true 表示 HEIC 或视频的 mtime/大小变了，需要重新转换。
func (s *Store) UpsertPair(ctx context.Context, p *Pair) (id int64, created bool, changed bool, err error) {
	now := Now()
	row := s.db.QueryRowContext(ctx,
		`SELECT `+pairColumns+` FROM live_pairs WHERE source_root=? AND heic_asset_id=? AND video_asset_id=?`,
		p.SourceRoot, p.HeicAssetID, p.VideoAssetID)
	prev, qerr := scanPair(row)
	switch {
	case errors.Is(qerr, sql.ErrNoRows):
		res, ierr := s.db.ExecContext(ctx, `INSERT INTO live_pairs
			(source_root, content_id, heic_asset_id, video_asset_id, heic_rel_path, video_rel_path,
			 heic_mtime, video_mtime, heic_size, video_size, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			p.SourceRoot, p.ContentID, p.HeicAssetID, p.VideoAssetID, p.HeicRelPath, p.VideoRelPath,
			p.HeicMtime, p.VideoMtime, p.HeicSize, p.VideoSize, now, now)
		if ierr != nil {
			return 0, false, false, fmt.Errorf("插入配对失败: %w", ierr)
		}
		newID, _ := res.LastInsertId()
		p.ID = newID
		p.CreatedAt, p.UpdatedAt = now, now
		return newID, true, false, nil
	case qerr != nil:
		return 0, false, false, fmt.Errorf("查询配对失败: %w", qerr)
	}

	changed = prev.HeicMtime != p.HeicMtime || prev.VideoMtime != p.VideoMtime ||
		prev.HeicSize != p.HeicSize || prev.VideoSize != p.VideoSize ||
		prev.HeicRelPath != p.HeicRelPath || prev.VideoRelPath != p.VideoRelPath

	if _, err := s.db.ExecContext(ctx, `UPDATE live_pairs SET
			content_id=?, heic_rel_path=?, video_rel_path=?,
			heic_mtime=?, video_mtime=?, heic_size=?, video_size=?, updated_at=?
		WHERE id=?`,
		p.ContentID, p.HeicRelPath, p.VideoRelPath, p.HeicMtime, p.VideoMtime,
		p.HeicSize, p.VideoSize, now, prev.ID); err != nil {
		return 0, false, false, fmt.Errorf("更新配对失败: %w", err)
	}
	p.ID = prev.ID
	p.CreatedAt = prev.CreatedAt
	p.UpdatedAt = now
	return prev.ID, false, changed, nil
}

// GetPair 按 id 读取配对。
func (s *Store) GetPair(ctx context.Context, id int64) (Pair, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+pairColumns+` FROM live_pairs WHERE id=?`, id)
	return scanPair(row)
}

// CountPairs 统计「有效」配对：两侧源文件都还在的配对。
// 源文件被移走/删除后配对记录仍会保留（便于移回时免于重转），
// 但不应计入仪表盘，否则数字会虚高。
func (s *Store) CountPairs(ctx context.Context, sourceRoot string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM live_pairs p
		JOIN live_assets h ON h.id = p.heic_asset_id
		JOIN live_assets v ON v.id = p.video_asset_id
		WHERE p.source_root=? AND h.state<>? AND v.state<>?`,
		sourceRoot, AssetStateMissing, AssetStateMissing).Scan(&n)
	return n, err
}
