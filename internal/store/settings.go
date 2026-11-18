package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/immich2motion/immich2motion/internal/config"
)

const settingsKey = "app_settings"

// EnsureSettings 读取设置；不存在则用 def 初始化并落库。
func (s *Store) EnsureSettings(ctx context.Context, def config.Settings) (config.Settings, error) {
	st, err := s.LoadSettings(ctx)
	if err == nil {
		return st, nil
	}
	if !errors.Is(err, errNoSettings) {
		return config.Settings{}, err
	}
	def.Normalize()
	if err := s.SaveSettings(ctx, def); err != nil {
		return config.Settings{}, err
	}
	return def, nil
}

// LoadSettings 读取运行时设置。
func (s *Store) LoadSettings(ctx context.Context) (config.Settings, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, settingsKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return config.Settings{}, errNoSettings
	}
	if err != nil {
		return config.Settings{}, err
	}
	var st config.Settings
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return config.Settings{}, fmt.Errorf("解析设置失败: %w", err)
	}
	st.Normalize()
	return st, nil
}

// SaveSettings 覆盖写入运行时设置。
func (s *Store) SaveSettings(ctx context.Context, st config.Settings) error {
	st.Normalize()
	blob, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("序列化设置失败: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO settings(key, value, updated_at) VALUES(?,?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		settingsKey, string(blob), Now())
	if err != nil {
		return fmt.Errorf("保存设置失败: %w", err)
	}
	return nil
}
