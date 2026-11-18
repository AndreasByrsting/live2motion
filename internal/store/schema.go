package store

// schemaVersion 当前数据库结构版本，升级时按 migration 递增。
const schemaVersion = 1

// schemaSQL 建表语句。所有时间字段统一存 Unix 秒（INTEGER）。
//
// 设计要点：
//   - live_assets 一个源文件一行，(source_root, rel_path) 唯一，源目录变更不会互相污染；
//   - live_pairs 只存「HEIC + 视频」的完整配对，孤儿文件靠 assets 表反查，不占用配对表；
//   - jobs 与 pairs 一对一，重新处理 = 重置同一行，避免历史膨胀；
//   - motion_outputs 记录输出产物与其 MD5，便于后续完整性校验。
const schemaSQL = `
CREATE TABLE IF NOT EXISTS schema_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- 源文件清单：HEIC 与视频各一行
CREATE TABLE IF NOT EXISTS live_assets (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    source_root     TEXT    NOT NULL,
    rel_path        TEXT    NOT NULL,
    kind            TEXT    NOT NULL CHECK (kind IN ('heic','video')),
    size_bytes      INTEGER NOT NULL DEFAULT 0,
    mtime_unix      INTEGER NOT NULL DEFAULT 0,
    content_id      TEXT,
    cid_source      TEXT,
    md5             TEXT,
    md5_mtime_unix  INTEGER,
    md5_checked_at  INTEGER,
    state           TEXT    NOT NULL DEFAULT 'ok',
    warning         TEXT,
    first_seen_at   INTEGER NOT NULL,
    last_seen_at    INTEGER NOT NULL,
    last_scan_id    INTEGER NOT NULL DEFAULT 0,
    updated_at      INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_assets_root_path ON live_assets(source_root, rel_path);
CREATE INDEX IF NOT EXISTS ix_assets_cid   ON live_assets(source_root, content_id);
CREATE INDEX IF NOT EXISTS ix_assets_state ON live_assets(source_root, state);

-- 配对结果：一行 = 一组可信的 Live Photo
CREATE TABLE IF NOT EXISTS live_pairs (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    source_root    TEXT    NOT NULL,
    content_id     TEXT    NOT NULL,
    heic_asset_id  INTEGER NOT NULL REFERENCES live_assets(id) ON DELETE CASCADE,
    video_asset_id INTEGER NOT NULL REFERENCES live_assets(id) ON DELETE CASCADE,
    heic_rel_path  TEXT    NOT NULL,
    video_rel_path TEXT    NOT NULL,
    heic_mtime     INTEGER NOT NULL DEFAULT 0,
    video_mtime    INTEGER NOT NULL DEFAULT 0,
    heic_size      INTEGER NOT NULL DEFAULT 0,
    video_size     INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    UNIQUE (source_root, heic_asset_id, video_asset_id)
);
CREATE INDEX IF NOT EXISTS ix_pairs_cid ON live_pairs(source_root, content_id);

-- 转换任务：与配对一对一
CREATE TABLE IF NOT EXISTS jobs (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    pair_id      INTEGER NOT NULL UNIQUE REFERENCES live_pairs(id) ON DELETE CASCADE,
    status       TEXT    NOT NULL DEFAULT 'pending',
    stage        TEXT,
    progress     REAL    NOT NULL DEFAULT 0,
    attempts     INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 3,
    last_error   TEXT,
    next_retry_at INTEGER,
    started_at   INTEGER,
    finished_at  INTEGER,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_jobs_status ON jobs(status);

-- 输出产物
CREATE TABLE IF NOT EXISTS motion_outputs (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    pair_id       INTEGER NOT NULL REFERENCES live_pairs(id) ON DELETE CASCADE,
    job_id        INTEGER,
    source_root   TEXT    NOT NULL,
    rel_path      TEXT    NOT NULL,
    abs_path      TEXT    NOT NULL,
    size_bytes    INTEGER NOT NULL,
    jpg_bytes     INTEGER NOT NULL,
    video_bytes   INTEGER NOT NULL,
    md5           TEXT,
    xmp_verified  INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,
    verified_at   INTEGER,
    UNIQUE (source_root, rel_path)
);
CREATE INDEX IF NOT EXISTS ix_outputs_pair ON motion_outputs(pair_id);

-- 运行日志（落库，重启后仍可回看）
CREATE TABLE IF NOT EXISTS logs (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    ts      INTEGER NOT NULL,
    level   TEXT    NOT NULL,
    scope   TEXT,
    message TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_logs_ts    ON logs(ts);
CREATE INDEX IF NOT EXISTS ix_logs_level ON logs(level, ts);

-- 扫描批次
CREATE TABLE IF NOT EXISTS scans (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    source_root    TEXT    NOT NULL,
    trigger        TEXT    NOT NULL,
    started_at     INTEGER NOT NULL,
    finished_at    INTEGER,
    files_seen     INTEGER NOT NULL DEFAULT 0,
    heic_seen      INTEGER NOT NULL DEFAULT 0,
    video_seen     INTEGER NOT NULL DEFAULT 0,
    pairs_total    INTEGER NOT NULL DEFAULT 0,
    pairs_new      INTEGER NOT NULL DEFAULT 0,
    pairs_unpaired INTEGER NOT NULL DEFAULT 0,
    warnings       INTEGER NOT NULL DEFAULT 0,
    error          TEXT
);
CREATE INDEX IF NOT EXISTS ix_scans_started ON scans(started_at);

-- 键值设置
CREATE TABLE IF NOT EXISTS settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);

-- bit rot 校验事件（MD5 与 mtime 不一致时落一条，便于追溯）
CREATE TABLE IF NOT EXISTS integrity_events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    asset_id   INTEGER NOT NULL,
    rel_path   TEXT    NOT NULL,
    kind       TEXT    NOT NULL,
    old_md5    TEXT,
    new_md5    TEXT,
    detail     TEXT,
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_integrity_created ON integrity_events(created_at);
`
