package config

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// Settings 是运行期可修改的设置，持久化在 SQLite 的 settings 表，Web 页面可改。
type Settings struct {
	SourceDir           string   `json:"source_dir"`
	OutputDir           string   `json:"output_dir"`
	ScanIntervalSec     int      `json:"scan_interval_sec"`
	Workers             int      `json:"workers"`
	JPGQuality          int      `json:"jpg_quality"`
	EnableMD5           bool     `json:"enable_md5"`
	AutoScan            bool     `json:"auto_scan"`
	MaxRetries          int      `json:"max_retries"`
	OutputSuffix        string   `json:"output_suffix"`
	HeicExts            []string `json:"heic_exts"`
	VideoExts           []string `json:"video_exts"`
	DeepVerifyHours     int      `json:"deep_verify_hours"` // 0 表示关闭深度校验
	LogRetention        int      `json:"log_retention"`
	Paused              bool     `json:"paused"`                // 是否暂停后台任务
	ReconvertOnChange   bool     `json:"reconvert_on_change"`   // 源文件变化时是否自动重转
	PreserveOrientation bool     `json:"preserve_orientation"`  // 是否复制原始 EXIF Orientation（默认否，避免二次旋转）
	VerifyBeforeConvert bool     `json:"verify_before_convert"` // 转换前用 MD5 校验源文件完整性
	AutoTheme           bool     `json:"auto_theme"`            // 是否按本地时间自动切换日间/夜间配色

	// IncludeUnconverted 控制输出目录的「完整度」：
	//   false（默认）只输出转换成功的 Motion Photo，图库里其它照片不出现；
	//   true 输出完整图库——转换成功的 Live Photo 只出 JPG（原始 HEIC/MOV 留在源目录），
	//        其余没有参与转换的文件按源目录结构原样复制一份，
	//        这样输出给下游的图库既完整又不重复。
	IncludeUnconverted bool `json:"include_unconverted"`

	// MirrorSettleMin 是「未定型的源文件」在输出目录门外的观察期（分钟）。
	//
	// 输出目录一旦写出文件，下游（同步工具 / 相册服务）就会立刻取走，
	// 之后再删除并不能撤回已经上传的那一份。所以只允许写「终态」：
	// 刚出现、还没配对的 HEIC/MOV，以及刚失败、用户可能马上重试的配对，
	// 都要先观察一段时间再决定是复制原件还是等转换。0 表示不等待（旧行为）。
	MirrorSettleMin int `json:"mirror_settle_min"`

	// ---- Webhook 通知 ----
	WebhookEnabled bool   `json:"webhook_enabled"`
	WebhookURL     string `json:"webhook_url"`
	// 事件开关：开始 / 完成 / 失败 / 系统故障
	// 推送以「一轮扫描 → 队列排空」为单位，每个事件一轮最多一条，不再按任务逐条推。
	WebhookOnStart   bool `json:"webhook_on_start"`
	WebhookOnSuccess bool `json:"webhook_on_success"`
	WebhookOnFailure bool `json:"webhook_on_failure"`
	WebhookOnSystem  bool `json:"webhook_on_system"`
	// 汇总消息里最多列出多少条失败明细
	WebhookMaxItems int `json:"webhook_max_items"`
}

// DefaultSettings 返回安全默认值：并发 2，JPG 质量 100（原图），MD5 开启。
func DefaultSettings() Settings {
	return Settings{
		SourceDir:           "/source",
		OutputDir:           "/output",
		ScanIntervalSec:     300,
		Workers:             2,
		JPGQuality:          100,
		EnableMD5:           true,
		AutoScan:            true,
		MaxRetries:          3,
		OutputSuffix:        "-motion",
		HeicExts:            []string{".heic", ".heif"},
		VideoExts:           []string{".mov", ".mp4"},
		DeepVerifyHours:     0,
		LogRetention:        3000,
		Paused:              false,
		ReconvertOnChange:   true,
		PreserveOrientation: false,
		VerifyBeforeConvert: true,
		IncludeUnconverted:  false,
		MirrorSettleMin:     30,
		AutoTheme:           true,

		WebhookEnabled:   false,
		WebhookURL:       "",
		WebhookOnStart:   true, // 一轮扫描只推一条，默认开启
		WebhookOnSuccess: true,
		WebhookOnFailure: true,
		WebhookOnSystem:  true,
		WebhookMaxItems:  10,
	}
}

// Normalize 修正越界值并补齐空字段，保证引擎拿到的设置一定合法。
func (s *Settings) Normalize() {
	if s.Workers < 1 {
		s.Workers = 1
	}
	if s.Workers > 16 {
		s.Workers = 16
	}
	if s.JPGQuality < 1 {
		s.JPGQuality = 100
	}
	if s.JPGQuality > 100 {
		s.JPGQuality = 100
	}
	if s.ScanIntervalSec < 10 {
		s.ScanIntervalSec = 10
	}
	if s.MaxRetries < 0 {
		s.MaxRetries = 0
	}
	if s.MaxRetries > 20 {
		s.MaxRetries = 20
	}
	if strings.TrimSpace(s.OutputSuffix) == "" {
		s.OutputSuffix = "-motion"
	}
	s.OutputSuffix = strings.TrimSpace(s.OutputSuffix)
	if s.LogRetention < 100 {
		s.LogRetention = 3000
	}
	if s.DeepVerifyHours < 0 {
		s.DeepVerifyHours = 0
	}
	// 观察期上限 24 小时：再长就失去意义了，还会让图库长期缺文件。
	// 负数按 0 处理（0 = 不等待）。「旧库缺字段」的补默认值在 store.LoadSettings 里做，
	// 这里不能把 0 也改成 30，否则用户主动填 0（立即复制）会被悄悄改掉。
	if s.MirrorSettleMin < 0 {
		s.MirrorSettleMin = 0
	}
	if s.MirrorSettleMin > 1440 {
		s.MirrorSettleMin = 1440
	}
	s.HeicExts = normalizeExts(s.HeicExts, []string{".heic", ".heif"})
	s.VideoExts = normalizeExts(s.VideoExts, []string{".mov", ".mp4"})
	s.SourceDir = filepath.Clean(strings.TrimSpace(s.SourceDir))
	s.OutputDir = filepath.Clean(strings.TrimSpace(s.OutputDir))

	// Webhook
	s.WebhookURL = strings.TrimSpace(s.WebhookURL)
	if s.WebhookMaxItems < 1 {
		s.WebhookMaxItems = 10
	}
	if s.WebhookMaxItems > 50 {
		s.WebhookMaxItems = 50
	}
	// 没有 URL 就不可能是启用状态，避免出现「开着但不发」的迷惑配置
	if s.WebhookURL == "" {
		s.WebhookEnabled = false
	}
}

// Validate 校验目录配置，重点是禁止输出目录落在源目录内部（源目录必须只读）。
func (s Settings) Validate() error {
	if s.SourceDir == "" || s.SourceDir == "." {
		return fmt.Errorf("源目录不能为空")
	}
	if s.OutputDir == "" || s.OutputDir == "." {
		return fmt.Errorf("输出目录不能为空")
	}
	if samePath(s.SourceDir, s.OutputDir) {
		return fmt.Errorf("输出目录不能与源目录相同（源目录必须只读）")
	}
	if isSubPath(s.OutputDir, s.SourceDir) {
		return fmt.Errorf("输出目录 %s 位于源目录 %s 内部，会污染图库并导致重复扫描", s.OutputDir, s.SourceDir)
	}
	if isSubPath(s.SourceDir, s.OutputDir) {
		return fmt.Errorf("源目录 %s 位于输出目录 %s 内部，请调整挂载路径", s.SourceDir, s.OutputDir)
	}
	if s.WebhookEnabled {
		if s.WebhookURL == "" {
			return fmt.Errorf("启用 Webhook 时必须填写推送地址")
		}
		if !strings.HasPrefix(s.WebhookURL, "http://") && !strings.HasPrefix(s.WebhookURL, "https://") {
			return fmt.Errorf("Webhook 地址必须以 http:// 或 https:// 开头")
		}
	}
	return nil
}

// ScanInterval 返回扫描间隔（秒）。
func (s Settings) ScanInterval() int { return s.ScanIntervalSec }

// HasHeicExt 判断扩展名是否属于 HEIC 候选集。
func (s Settings) HasHeicExt(ext string) bool { return containsExt(s.HeicExts, ext) }

// HasVideoExt 判断扩展名是否属于视频候选集。
func (s Settings) HasVideoExt(ext string) bool { return containsExt(s.VideoExts, ext) }

func normalizeExts(in, def []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, e := range in {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		if _, ok := seen[e]; ok {
			continue
		}
		seen[e] = struct{}{}
		out = append(out, e)
	}
	if len(out) == 0 {
		out = append(out, def...)
	}
	sort.Strings(out)
	return out
}

func containsExt(list []string, ext string) bool {
	ext = strings.ToLower(ext)
	for _, e := range list {
		if e == ext {
			return true
		}
	}
	return false
}

func samePath(a, b string) bool {
	return strings.EqualFold(filepath.ToSlash(filepath.Clean(a)), filepath.ToSlash(filepath.Clean(b)))
}

// isSubPath 判断 child 是否位于 parent 内部（child == parent 也返回 true）。
func isSubPath(child, parent string) bool {
	child = filepath.ToSlash(filepath.Clean(child))
	parent = filepath.ToSlash(filepath.Clean(parent))
	if strings.EqualFold(child, parent) {
		return true
	}
	return strings.HasPrefix(strings.ToLower(child)+"/", strings.ToLower(parent)+"/")
}
