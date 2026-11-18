// Package config 定义启动期不可变配置（环境变量 + 命令行）与运行期可改设置（持久化在 SQLite）。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Version 由构建时 -ldflags 注入，默认开发版本。
var Version = "dev"

// Config 启动期不可变配置。只从环境变量读取，Web 页面无法修改。
type Config struct {
	DataDir  string // /data 持久化目录
	DBPath   string // SQLite 文件路径
	WebAddr  string // Web 监听地址
	LogLevel string // info / warn / error

	HeifBin string // heif-convert 可执行文件
	ExifBin string // exiftool 可执行文件

	// 命令行模式
	Web      bool // --web 启动常驻 Web 服务
	ScanOnce bool // --scan-once 扫描一次后退出
	DryRun   bool // --dry-run 只扫描配对不转换
}

// FromEnv 读取环境变量并填充默认值。
func FromEnv() Config {
	c := Config{
		DataDir:  env("I2M_DATA_DIR", "/data"),
		WebAddr:  env("I2M_WEB_ADDR", ":8080"),
		LogLevel: strings.ToLower(env("I2M_LOG_LEVEL", "info")),
		HeifBin:  env("I2M_HEIF_CONVERT", "heif-convert"),
		ExifBin:  env("I2M_EXIFTOOL", "exiftool"),
	}
	c.DBPath = env("I2M_DB_PATH", filepath.Join(c.DataDir, "immich2motion.db"))
	return c
}

// EnsureDataDir 创建数据目录，SQLite 文件必须落在可写卷上。
func (c Config) EnsureDataDir() error {
	if err := os.MkdirAll(c.DataDir, 0o755); err != nil {
		return fmt.Errorf("创建数据目录 %s 失败: %w", c.DataDir, err)
	}
	return nil
}

// NewSettingsFromEnv 用环境变量初始化默认设置（仅首次建库时使用）。
func NewSettingsFromEnv() Settings {
	s := DefaultSettings()
	s.SourceDir = env("I2M_SOURCE_DIR", "/source")
	s.OutputDir = env("I2M_OUTPUT_DIR", "/output")

	if v, ok := envInt("I2M_WORKERS"); ok {
		s.Workers = v
	}
	if v, ok := envInt("I2M_JPG_QUALITY"); ok {
		s.JPGQuality = v
	}
	if v, ok := envInt("I2M_SCAN_INTERVAL_SEC"); ok {
		s.ScanIntervalSec = v
	}
	if v, ok := envBool("I2M_ENABLE_MD5"); ok {
		s.EnableMD5 = v
	}
	if v, ok := envBool("I2M_AUTO_SCAN"); ok {
		s.AutoScan = v
	}
	if v, ok := envInt("I2M_MAX_RETRIES"); ok {
		s.MaxRetries = v
	}
	if v := env("I2M_OUTPUT_SUFFIX", ""); v != "" {
		s.OutputSuffix = v
	}
	if v, ok := envBool("I2M_INCLUDE_UNCONVERTED"); ok {
		s.IncludeUnconverted = v
	}
	s.Normalize()
	return s
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string) (int, bool) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return 0, false
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return v, true
}

func envBool(key string) (bool, bool) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return false, false
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, false
	}
	return v, true
}
