package store

import (
	"context"
	"database/sql"
	"errors"
)

// MirrorRecord 一条「原样复制到输出目录」的记录。
//
// 之所以要落库而不是靠 stat 现场判断：清理副本时（源文件消失、或转换成功后不再需要原件）
// 只有存在记录才敢删文件。输出目录归下游与用户共用，误删比多留一份危险得多。
type MirrorRecord struct {
	RelPath   string
	SizeBytes int64
	MtimeUnix int64
}

// ListMirrorRecords 读取某个源根目录下的全部镜像记录，按相对路径索引。
func (s *Store) ListMirrorRecords(ctx context.Context, sourceRoot string) (map[string]MirrorRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT rel_path, size_bytes, mtime_unix FROM mirror_outputs WHERE source_root=?`, sourceRoot)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]MirrorRecord)
	for rows.Next() {
		var r MirrorRecord
		if err := rows.Scan(&r.RelPath, &r.SizeBytes, &r.MtimeUnix); err != nil {
			return nil, err
		}
		out[r.RelPath] = r
	}
	return out, rows.Err()
}

// UpsertMirrorRecords 批量登记镜像记录（一个事务，避免逐条提交）。
func (s *Store) UpsertMirrorRecords(ctx context.Context, sourceRoot string, recs []MirrorRecord) error {
	if len(recs) == 0 {
		return nil
	}
	now := Now()
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO mirror_outputs
			(source_root, rel_path, size_bytes, mtime_unix, copied_at, updated_at)
			VALUES (?,?,?,?,?,?)
			ON CONFLICT(source_root, rel_path) DO UPDATE SET
			 size_bytes=excluded.size_bytes, mtime_unix=excluded.mtime_unix, updated_at=excluded.updated_at`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, r := range recs {
			if _, err := stmt.ExecContext(ctx, sourceRoot, r.RelPath, r.SizeBytes, r.MtimeUnix, now, now); err != nil {
				return err
			}
		}
		return nil
	})
}

// GetMirrorRecord 读取单条镜像记录。
func (s *Store) GetMirrorRecord(ctx context.Context, sourceRoot, relPath string) (MirrorRecord, bool, error) {
	var r MirrorRecord
	err := s.db.QueryRowContext(ctx,
		`SELECT rel_path, size_bytes, mtime_unix FROM mirror_outputs WHERE source_root=? AND rel_path=?`,
		sourceRoot, relPath).Scan(&r.RelPath, &r.SizeBytes, &r.MtimeUnix)
	if errors.Is(err, sql.ErrNoRows) {
		return MirrorRecord{}, false, nil
	}
	if err != nil {
		return MirrorRecord{}, false, err
	}
	return r, true, nil
}

// DeleteMirrorRecord 删除单条镜像记录。
func (s *Store) DeleteMirrorRecord(ctx context.Context, sourceRoot, relPath string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM mirror_outputs WHERE source_root=? AND rel_path=?`, sourceRoot, relPath)
	return err
}

// DeleteMirrorRecords 批量删除镜像记录（源文件消失后清理用）。
func (s *Store) DeleteMirrorRecords(ctx context.Context, sourceRoot string, relPaths []string) error {
	if len(relPaths) == 0 {
		return nil
	}
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx,
			`DELETE FROM mirror_outputs WHERE source_root=? AND rel_path=?`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, rel := range relPaths {
			if _, err := stmt.ExecContext(ctx, sourceRoot, rel); err != nil {
				return err
			}
		}
		return nil
	})
}

// CountMirrors 统计某个源根目录下已复制的文件数量。
func (s *Store) CountMirrors(ctx context.Context, sourceRoot string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mirror_outputs WHERE source_root=?`, sourceRoot).Scan(&n)
	return n, err
}

// MirrorBytes 统计已复制文件的总字节数。
func (s *Store) MirrorBytes(ctx context.Context, sourceRoot string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(size_bytes),0) FROM mirror_outputs WHERE source_root=?`, sourceRoot).Scan(&n)
	return n, err
}
