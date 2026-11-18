package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Output 记录一个已生成的 Motion Photo 产物。
type Output struct {
	ID          int64
	PairID      int64
	JobID       int64
	SourceRoot  string
	RelPath     string
	AbsPath     string
	SizeBytes   int64
	JPGBytes    int64 // 等于 MicroVideoOffset
	VideoBytes  int64
	MD5         string
	XMPVerified bool
	CreatedAt   int64
	VerifiedAt  int64
}

// SaveOutput 写入或更新产物记录（按 source_root + rel_path 唯一）。
func (s *Store) SaveOutput(ctx context.Context, o *Output) error {
	now := Now()
	verified := 0
	if o.XMPVerified {
		verified = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO motion_outputs
		(pair_id, job_id, source_root, rel_path, abs_path, size_bytes, jpg_bytes, video_bytes,
		 md5, xmp_verified, created_at, verified_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(source_root, rel_path) DO UPDATE SET
		 pair_id=excluded.pair_id, job_id=excluded.job_id, abs_path=excluded.abs_path,
		 size_bytes=excluded.size_bytes, jpg_bytes=excluded.jpg_bytes, video_bytes=excluded.video_bytes,
		 md5=excluded.md5, xmp_verified=excluded.xmp_verified, verified_at=excluded.verified_at`,
		o.PairID, o.JobID, o.SourceRoot, o.RelPath, o.AbsPath, o.SizeBytes, o.JPGBytes, o.VideoBytes,
		o.MD5, verified, now, now)
	if err != nil {
		return fmt.Errorf("保存产物记录失败: %w", err)
	}
	return nil
}

// GetOutputByPair 读取某配对最新产物。
func (s *Store) GetOutputByPair(ctx context.Context, pairID int64) (*Output, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, pair_id, COALESCE(job_id,0), source_root, rel_path, abs_path,
		size_bytes, jpg_bytes, video_bytes, COALESCE(md5,''), xmp_verified, created_at, COALESCE(verified_at,0)
		FROM motion_outputs WHERE pair_id=? ORDER BY id DESC LIMIT 1`, pairID)
	var o Output
	var verified int
	err := row.Scan(&o.ID, &o.PairID, &o.JobID, &o.SourceRoot, &o.RelPath, &o.AbsPath,
		&o.SizeBytes, &o.JPGBytes, &o.VideoBytes, &o.MD5, &verified, &o.CreatedAt, &o.VerifiedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	o.XMPVerified = verified == 1
	return &o, nil
}

// CountOutputs 统计已生成产物数量。
func (s *Store) CountOutputs(ctx context.Context, sourceRoot string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM motion_outputs WHERE source_root=?`, sourceRoot).Scan(&n)
	return n, err
}
