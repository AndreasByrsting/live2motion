package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// 资产状态
const (
	AssetStateOK         = "ok"         // 可读，且已读到 ContentIdentifier
	AssetStateNoCID      = "no_cid"     // 可读，但没有 ContentIdentifier（普通照片/普通视频）
	AssetStateUnreadable = "unreadable" // 读取或解析失败
	AssetStateBitrot     = "bitrot"     // MD5 变化而 mtime 未变，疑似静默损坏
	AssetStateMissing    = "missing"    // 本轮扫描未发现（可能已删除）
)

// 资产类型
const (
	KindHEIC  = "heic"
	KindVideo = "video"
)

// Asset 对应 live_assets 表的一行。
type Asset struct {
	ID           int64
	SourceRoot   string
	RelPath      string
	Kind         string
	SizeBytes    int64
	MtimeUnix    int64
	ContentID    string
	CIDSource    string
	MD5          string
	MD5MtimeUnix int64
	MD5CheckedAt int64
	State        string
	Warning      string
	FirstSeenAt  int64
	LastSeenAt   int64
	LastScanID   int64
	UpdatedAt    int64
}

// dbtx 让同一套 SQL 既能跑在事务里，也能跑在连接池上。
type dbtx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const assetColumns = `id, source_root, rel_path, kind, size_bytes, mtime_unix,
	COALESCE(content_id,''), COALESCE(cid_source,''), COALESCE(md5,''),
	COALESCE(md5_mtime_unix,0), COALESCE(md5_checked_at,0), state,
	COALESCE(warning,''), first_seen_at, last_seen_at, last_scan_id, updated_at`

func scanAsset(row interface{ Scan(...any) error }) (Asset, error) {
	var a Asset
	err := row.Scan(&a.ID, &a.SourceRoot, &a.RelPath, &a.Kind, &a.SizeBytes, &a.MtimeUnix,
		&a.ContentID, &a.CIDSource, &a.MD5, &a.MD5MtimeUnix, &a.MD5CheckedAt, &a.State,
		&a.Warning, &a.FirstSeenAt, &a.LastSeenAt, &a.LastScanID, &a.UpdatedAt)
	return a, err
}

// UpsertAsset 登记一次源文件出现。只更新文件系统属性与"最近可见"信息，
// content_id 与 md5 由专门的 Set 方法维护，避免读取失败时被清空。
// 返回 created 表示新文件，prev 是更新前的记录（created 时为 ID=0 的空值）。
func (s *Store) UpsertAsset(ctx context.Context, a *Asset) (created bool, prev Asset, err error) {
	return upsertAsset(ctx, s.db, a)
}

func upsertAsset(ctx context.Context, q dbtx, a *Asset) (bool, Asset, error) {
	now := Now()
	row := q.QueryRowContext(ctx,
		`SELECT `+assetColumns+` FROM live_assets WHERE source_root=? AND rel_path=?`,
		a.SourceRoot, a.RelPath)
	prev, err := scanAsset(row)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		res, ierr := q.ExecContext(ctx, `INSERT INTO live_assets
			(source_root, rel_path, kind, size_bytes, mtime_unix, content_id, cid_source,
			 md5, md5_mtime_unix, md5_checked_at, state, warning,
			 first_seen_at, last_seen_at, last_scan_id, updated_at)
			VALUES (?,?,?,?,?,NULL,NULL,NULL,NULL,NULL,?,NULL,?,?,?,?)`,
			a.SourceRoot, a.RelPath, a.Kind, a.SizeBytes, a.MtimeUnix, a.State, now, now, a.LastScanID, now)
		if ierr != nil {
			return false, Asset{}, fmt.Errorf("插入资产失败: %w", ierr)
		}
		id, _ := res.LastInsertId()
		a.ID = id
		a.FirstSeenAt, a.LastSeenAt, a.UpdatedAt = now, now, now
		return true, Asset{}, nil
	case err != nil:
		return false, Asset{}, fmt.Errorf("查询资产失败: %w", err)
	}

	// 已存在：刷新文件属性。state 只在之前不是终态(unreadable/bitrot)时更新，
	// 保留告警信息直到人工清除。
	newState := a.State
	if prev.State == AssetStateBitrot || prev.State == AssetStateUnreadable {
		if a.State == AssetStateOK {
			newState = prev.State // 保持告警态，等待重置流程处理
		}
	}
	if _, err := q.ExecContext(ctx, `UPDATE live_assets SET
			size_bytes=?, mtime_unix=?, state=?, last_seen_at=?, last_scan_id=?, updated_at=?
		WHERE id=?`,
		a.SizeBytes, a.MtimeUnix, newState, now, a.LastScanID, now, prev.ID); err != nil {
		return false, prev, fmt.Errorf("更新资产失败: %w", err)
	}
	a.ID = prev.ID
	a.State = newState
	a.FirstSeenAt = prev.FirstSeenAt
	a.LastSeenAt, a.UpdatedAt = now, now
	return false, prev, nil
}

// SetAssetContentID 写入 ContentIdentifier 与来源标记。
func (s *Store) SetAssetContentID(ctx context.Context, id int64, cid, source string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE live_assets SET content_id=NULLIF(?,''), cid_source=NULLIF(?,''), updated_at=? WHERE id=?`,
		cid, source, Now(), id)
	return err
}

// SetAssetMD5 写入 MD5 及其对应 mtime。
func (s *Store) SetAssetMD5(ctx context.Context, id int64, md5 string, mtimeUnix int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE live_assets SET md5=?, md5_mtime_unix=?, md5_checked_at=?, updated_at=? WHERE id=?`,
		md5, mtimeUnix, Now(), Now(), id)
	return err
}

// SetAssetState 更新状态与告警文本。
func (s *Store) SetAssetState(ctx context.Context, id int64, state, warning string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE live_assets SET state=?, warning=NULLIF(?,''), updated_at=? WHERE id=?`,
		state, warning, Now(), id)
	return err
}

// GetAsset 按 id 读取。
func (s *Store) GetAsset(ctx context.Context, id int64) (Asset, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+assetColumns+` FROM live_assets WHERE id=?`, id)
	return scanAsset(row)
}

// MarkMissingAssets 把本轮扫描未出现的资产标记为 missing，并返回受影响数量。
func (s *Store) MarkMissingAssets(ctx context.Context, sourceRoot string, scanID int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE live_assets SET state=?, updated_at=?
		WHERE source_root=? AND last_scan_id<>? AND state<>?`,
		AssetStateMissing, Now(), sourceRoot, scanID, AssetStateMissing)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListPairingCandidates 返回所有可用于配对的资产（有 ContentIdentifier 且状态正常）。
func (s *Store) ListPairingCandidates(ctx context.Context, sourceRoot string) ([]Asset, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+assetColumns+` FROM live_assets
		WHERE source_root=? AND content_id IS NOT NULL AND content_id<>''
		  AND state IN (?,?)
		ORDER BY rel_path`,
		sourceRoot, AssetStateOK, AssetStateBitrot)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Asset
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CountAssets 统计指定类型与状态的资产数量。
func (s *Store) CountAssets(ctx context.Context, sourceRoot, kind, state string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM live_assets WHERE source_root=? AND kind=? AND state=?`,
		sourceRoot, kind, state).Scan(&n)
	return n, err
}

// UnpairedCount 统计「有 ContentIdentifier 但没有配对」的资产（孤儿）。
type UnpairedCount struct {
	Heic  int64
	Video int64
}

// CountUnpaired 用 content_id 反查配对表，统计孤儿文件。
func (s *Store) CountUnpaired(ctx context.Context, sourceRoot string) (UnpairedCount, error) {
	var out UnpairedCount
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.kind, COUNT(*) FROM live_assets a
		WHERE a.source_root=? AND a.content_id IS NOT NULL AND a.content_id<>''
		  AND a.state IN (?,?)
		  AND NOT EXISTS (
			SELECT 1 FROM live_pairs p
			WHERE p.source_root=a.source_root
			  AND (p.heic_asset_id=a.id OR p.video_asset_id=a.id)
		  )
		GROUP BY a.kind`, sourceRoot, AssetStateOK, AssetStateBitrot)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var n int64
		if err := rows.Scan(&kind, &n); err != nil {
			return out, err
		}
		switch kind {
		case KindHEIC:
			out.Heic = n
		case KindVideo:
			out.Video = n
		}
	}
	return out, rows.Err()
}

// ListBitrotAssets 列出疑似静默损坏的资产，仪表盘提示用。
func (s *Store) ListBitrotAssets(ctx context.Context, limit int) ([]Asset, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+assetColumns+` FROM live_assets
		WHERE state=? ORDER BY updated_at DESC LIMIT ?`, AssetStateBitrot, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Asset
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListSettledUnpairedAssets 列出「已过观察期、且从未配上对」的源文件相对路径。
//
// 这类文件是完整图库模式里唯一允许被原样复制出去的转换类文件：
// 它们等了足够久也没等到另一半，可以认定不会被合成成 Motion Photo。
// before 是观察期的时间下界（first_seen_at <= before 才算定型）。
func (s *Store) ListSettledUnpairedAssets(ctx context.Context, sourceRoot string, before int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.rel_path FROM live_assets a
		WHERE a.source_root = ? AND a.first_seen_at <= ? AND a.state <> ?
		  AND NOT EXISTS (
		      SELECT 1 FROM live_pairs p
		      WHERE p.heic_asset_id = a.id OR p.video_asset_id = a.id
		  )
		ORDER BY a.rel_path`, sourceRoot, before, AssetStateMissing)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var rel string
		if err := rows.Scan(&rel); err != nil {
			return nil, err
		}
		out = append(out, rel)
	}
	return out, rows.Err()
}

// ListUnsettledAssetPaths 列出「刚刚出现、且还没有配对」的源文件相对路径。
//
// 这是完整图库模式的观察期依据：新到的 HEIC/MOV 可能只是 Live Photo 的一半，
// 另一半几分钟后才被同步/导入。此时把这一半原样写进输出目录，
// 等配对成功、转换完成后又得把它删掉——对 Syncthing / Immich 这种
// 「看到即上传」的下游来说，删掉并不能撤回已经上传的那一份，重复就此产生。
//
// 因此这类文件在 first_seen_at + 观察期之前一律不复制，等它定型。
func (s *Store) ListUnsettledAssetPaths(ctx context.Context, sourceRoot string, since int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.rel_path FROM live_assets a
		WHERE a.source_root = ? AND a.first_seen_at >= ? AND a.state <> ?
		  AND NOT EXISTS (
		      SELECT 1 FROM live_pairs p
		      WHERE p.heic_asset_id = a.id OR p.video_asset_id = a.id
		  )
		ORDER BY a.rel_path`, sourceRoot, since, AssetStateMissing)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var rel string
		if err := rows.Scan(&rel); err != nil {
			return nil, err
		}
		out = append(out, rel)
	}
	return out, rows.Err()
}
