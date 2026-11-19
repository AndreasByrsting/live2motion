package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/AndreasByrsting/live2motion/internal/config"
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
	// 旧库的 JSON 里没有 auto_theme 字段，解析出来是 false。
	// 但旧版本的界面本来就是「按时间自动切换」，所以升级时要补成 true，
	// 否则升级后所有人的面板会突然固定在夜间配色。
	// mirror_settle_min 同理：旧库没有这个字段，解析成 0 = 立即复制（旧行为），
	// 而新默认值是 30 分钟观察期，升级后应当直接生效。
	var probe map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &probe) == nil {
		if _, ok := probe["auto_theme"]; !ok {
			st.AutoTheme = true
		}
		if _, ok := probe["mirror_settle_min"]; !ok {
			st.MirrorSettleMin = config.DefaultSettings().MirrorSettleMin
		}
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
