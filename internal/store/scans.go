package store

import (
	"context"
	"database/sql"
	"errors"
)

// ScanRun 一次扫描批次的统计。
type ScanRun struct {
	ID            int64
	SourceRoot    string
	Trigger       string
	StartedAt     int64
	FinishedAt    int64
	FilesSeen     int64
	HeicSeen      int64
	VideoSeen     int64
	PairsTotal    int64
	PairsNew      int64
	PairsUnpaired int64
	Warnings      int64
	Error         string
}

// StartScan 创建扫描批次，返回批次 id（用于标记本轮见到的文件）。
func (s *Store) StartScan(ctx context.Context, sourceRoot, trigger string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO scans(source_root, trigger, started_at) VALUES(?,?,?)`,
		sourceRoot, trigger, Now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishScan 回写扫描结果。
func (s *Store) FinishScan(ctx context.Context, run ScanRun) error {
	_, err := s.db.ExecContext(ctx, `UPDATE scans SET
		finished_at=?, files_seen=?, heic_seen=?, video_seen=?, pairs_total=?, pairs_new=?,
		pairs_unpaired=?, warnings=?, error=NULLIF(?,'')
		WHERE id=?`,
		Now(), run.FilesSeen, run.HeicSeen, run.VideoSeen, run.PairsTotal, run.PairsNew,
		run.PairsUnpaired, run.Warnings, run.Error, run.ID)
	return err
}

// LastScan 读取最近一次已完成的扫描。
func (s *Store) LastScan(ctx context.Context) (*ScanRun, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, source_root, trigger, started_at, COALESCE(finished_at,0),
		files_seen, heic_seen, video_seen, pairs_total, pairs_new, pairs_unpaired, warnings, COALESCE(error,'')
		FROM scans ORDER BY id DESC LIMIT 1`)
	var r ScanRun
	err := row.Scan(&r.ID, &r.SourceRoot, &r.Trigger, &r.StartedAt, &r.FinishedAt,
		&r.FilesSeen, &r.HeicSeen, &r.VideoSeen, &r.PairsTotal, &r.PairsNew, &r.PairsUnpaired,
		&r.Warnings, &r.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// IntegrityEvent 记录一次完整性事件（如 MD5 变化但 mtime 未变）。
type IntegrityEvent struct {
	ID        int64
	AssetID   int64
	RelPath   string
	Kind      string
	OldMD5    string
	NewMD5    string
	Detail    string
	CreatedAt int64
}

// AddIntegrityEvent 写入完整性事件。
func (s *Store) AddIntegrityEvent(ctx context.Context, e IntegrityEvent) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO integrity_events
		(asset_id, rel_path, kind, old_md5, new_md5, detail, created_at) VALUES(?,?,?,?,?,?,?)`,
		e.AssetID, e.RelPath, e.Kind, e.OldMD5, e.NewMD5, e.Detail, Now())
	return err
}

// ListIntegrityEvents 读取最近事件。
func (s *Store) ListIntegrityEvents(ctx context.Context, limit int) ([]IntegrityEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, asset_id, rel_path, kind,
		COALESCE(old_md5,''), COALESCE(new_md5,''), COALESCE(detail,''), created_at
		FROM integrity_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IntegrityEvent
	for rows.Next() {
		var e IntegrityEvent
		if err := rows.Scan(&e.ID, &e.AssetID, &e.RelPath, &e.Kind, &e.OldMD5, &e.NewMD5, &e.Detail, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ResetBitrot 清除单个资产的 bitrot 告警（人工确认后调用）。
func (s *Store) ResetBitrot(ctx context.Context, assetID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE live_assets SET state=?, warning=NULL, updated_at=? WHERE id=? AND state=?`,
		AssetStateOK, Now(), assetID, AssetStateBitrot)
	return err
}
