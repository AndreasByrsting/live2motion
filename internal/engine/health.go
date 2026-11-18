package engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/immich2motion/immich2motion/internal/config"
	"github.com/immich2motion/immich2motion/internal/library"
	"github.com/immich2motion/immich2motion/internal/webhook"
)

// CheckResult 单条环境检查结果。
type CheckResult struct {
	Key      string `json:"key"`
	Name     string `json:"name"`
	OK       bool   `json:"ok"`
	Detail   string `json:"detail"`
	Hint     string `json:"hint"`
	Blocking bool   `json:"blocking"` // true 表示该项不通过就无法转换
}

// HealthReport 环境自检汇总。
type HealthReport struct {
	OK        bool          `json:"ok"`    // 全部通过
	Ready     bool          `json:"ready"` // 所有「阻断项」通过，可以开始转换
	CheckedAt int64         `json:"checked_at"`
	Checks    []CheckResult `json:"checks"`
}

// Problems 返回未通过的检查项。
func (r HealthReport) Problems() []CheckResult {
	out := make([]CheckResult, 0, len(r.Checks))
	for _, c := range r.Checks {
		if !c.OK {
			out = append(out, c)
		}
	}
	return out
}

// CheckHealth 执行一次环境自检：目录状态 + 外部工具可用性。
//
// 目录策略：
//   - 数据目录、输出目录是「我们自己的产出位置」，不存在就创建；
//   - 源目录是用户的图库，**绝不自动创建**——自动建一个空目录只会掩盖挂载/路径错误，
//     让用户看到「扫描成功 0 个文件」而不是「路径不对」。
func (e *Engine) CheckHealth(ctx context.Context) HealthReport {
	st := e.Settings()
	rep := HealthReport{OK: true, Ready: true, CheckedAt: time.Now().Unix()}

	add := func(c CheckResult) {
		if !c.OK {
			rep.OK = false
			if c.Blocking {
				rep.Ready = false
			}
		}
		rep.Checks = append(rep.Checks, c)
	}

	// ---- 源目录：只检查，不创建 ----
	if fi, err := os.Stat(st.SourceDir); err != nil {
		detail := fmt.Sprintf("无法访问 %s", st.SourceDir)
		if os.IsNotExist(err) {
			detail = fmt.Sprintf("源目录不存在: %s", st.SourceDir)
		} else {
			detail = fmt.Sprintf("无法访问 %s: %v", st.SourceDir, err)
		}
		add(CheckResult{
			Key: "source", Name: "源目录", OK: false, Detail: detail, Blocking: false,
			Hint: "容器：确认 -v /你的图库:/source:ro 挂载正确；本地运行：把 I2M_SOURCE_DIR 指向真实目录。" +
				"程序不会自动创建源目录，以免掩盖路径错误。",
		})
	} else if !fi.IsDir() {
		add(CheckResult{
			Key: "source", Name: "源目录", OK: false,
			Detail: fmt.Sprintf("源路径不是目录: %s", st.SourceDir), Blocking: false,
			Hint: "请把源目录配置指向图库的目录，而不是单个文件。",
		})
	} else if err := probeReadable(st.SourceDir); err != nil {
		add(CheckResult{
			Key: "source", Name: "源目录", OK: false,
			Detail: fmt.Sprintf("源目录不可读: %v", err), Blocking: false,
			Hint: "检查挂载权限；容器以 root 运行时通常可读，降权运行时需保证 UID 有读权限。",
		})
	} else {
		add(CheckResult{Key: "source", Name: "源目录", OK: true, Detail: st.SourceDir})
	}

	// ---- 输出目录：不存在就创建 ----
	if err := os.MkdirAll(st.OutputDir, 0o755); err != nil {
		add(CheckResult{
			Key: "output", Name: "输出目录", OK: false, Blocking: true,
			Detail: fmt.Sprintf("创建失败 %s: %v", st.OutputDir, err),
			Hint:   "确认宿主目录已挂载到 /output 且可写。",
		})
	} else if err := probeWritable(st.OutputDir); err != nil {
		add(CheckResult{
			Key: "output", Name: "输出目录", OK: false, Blocking: true,
			Detail: fmt.Sprintf("不可写 %s: %v", st.OutputDir, err),
			Hint:   "调整目录权限，或在 compose 中用 user: \"UID:GID\" 以有写权限的身份运行。",
		})
	} else {
		add(CheckResult{Key: "output", Name: "输出目录", OK: true, Detail: st.OutputDir})
	}

	// ---- 数据目录（未配置时跳过：说明调用方自己管理 SQLite 路径）----
	if e.cfg.DataDir == "" {
		add(CheckResult{Key: "data", Name: "数据目录", OK: true, Detail: "由调用方指定"})
	} else if err := os.MkdirAll(e.cfg.DataDir, 0o755); err != nil {
		add(CheckResult{
			Key: "data", Name: "数据目录", OK: false, Blocking: true,
			Detail: fmt.Sprintf("创建失败 %s: %v", e.cfg.DataDir, err),
			Hint:   "确保 /data 已挂载且可写，否则 SQLite 无法持久化。",
		})
	} else if err := probeWritable(e.cfg.DataDir); err != nil {
		add(CheckResult{
			Key: "data", Name: "数据目录", OK: false, Blocking: true,
			Detail: fmt.Sprintf("不可写 %s: %v", e.cfg.DataDir, err),
			Hint:   "确保 /data 挂载点可写。",
		})
	} else {
		add(CheckResult{Key: "data", Name: "数据目录", OK: true, Detail: e.cfg.DataDir})
	}

	// ---- 外部工具 ----
	if ver, err := library.ProbeExifTool(ctx, e.cfg.ExifBin); err != nil {
		add(CheckResult{
			Key: "exiftool", Name: "exiftool", OK: false, Blocking: true,
			Detail: fmt.Sprintf("不可用: %v", err),
			Hint:   "容器镜像已内置 exiftool；本地运行请安装并确保在 PATH 中，或用 I2M_EXIFTOOL 指定绝对路径。",
		})
	} else {
		add(CheckResult{Key: "exiftool", Name: "exiftool", OK: true, Detail: "版本 " + ver})
	}

	if ver, err := library.ProbeHeifConvert(ctx, e.cfg.HeifBin); err != nil {
		add(CheckResult{
			Key: "heif-convert", Name: "heif-convert", OK: false, Blocking: true,
			Detail: fmt.Sprintf("不可用: %v", err),
			Hint:   "容器镜像已内置（libheif-examples）；本地运行请安装 libheif，或用 I2M_HEIF_CONVERT 指定绝对路径。",
		})
	} else {
		add(CheckResult{Key: "heif-convert", Name: "heif-convert", OK: true, Detail: "版本 " + ver})
	}

	return rep
}

// Health 返回最近一次自检结果。
func (e *Engine) Health() HealthReport {
	e.healthMu.RLock()
	defer e.healthMu.RUnlock()
	return e.health
}

// ToolsReady 转换所需的外部依赖是否就绪。
func (e *Engine) ToolsReady() bool { return e.toolsReady.Load() }

// Recheck 手动重新执行环境自检（修好挂载或装好工具后点一下即可恢复转换）。
func (e *Engine) Recheck(ctx context.Context) HealthReport {
	rep := e.refreshHealth(ctx)
	e.wakeAll()
	return rep
}

// refreshHealth 重新自检并同步「能否转换」的开关。
// 工具缺失时闸门关闭，worker 不会去领任务，避免把整个队列烧成 failed。
func (e *Engine) refreshHealth(ctx context.Context) HealthReport {
	rep := e.CheckHealth(ctx)

	e.healthMu.Lock()
	e.health = rep
	e.healthMu.Unlock()

	wasReady := e.toolsReady.Swap(rep.Ready)
	e.reportHealthTransition(rep, wasReady)
	return rep
}

// reportHealthTransition 只在状态变化时打日志，避免刷屏。
func (e *Engine) reportHealthTransition(rep HealthReport, wasReady bool) {
	nowReady := rep.Ready
	if nowReady && !wasReady {
		e.Info("engine", "环境自检通过，转换已就绪")
		return
	}
	if !nowReady && wasReady {
		e.Error("engine", "环境自检未通过，已暂停转换（任务保持待处理，修复后会自动恢复）")
		if probs := rep.Problems(); len(probs) > 0 {
			lines := make([]string, 0, len(probs))
			for _, p := range probs {
				lines = append(lines, p.Name+"："+p.Detail)
			}
			e.notifier.Notify(webhook.Event{
				Kind:   webhook.KindSystem,
				Title:  "环境异常，转换已暂停",
				Detail: strings.Join(lines, "；"),
			})
		}
	}
	if !nowReady {
		for _, c := range rep.Problems() {
			if c.Blocking {
				e.Error("engine", "[%s] %s —— %s", c.Name, c.Detail, c.Hint)
			} else {
				e.Warn("engine", "[%s] %s —— %s", c.Name, c.Detail, c.Hint)
			}
		}
	}
	// 目录/工具问题在首次启动时也要完整提示一次
	if nowReady && !wasReady {
		for _, c := range rep.Checks {
			e.Info("engine", "[%s] %s", c.Name, c.Detail)
		}
	}
}

// probeWritable 通过创建并删除临时文件确认目录可写。
func probeWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".i2m-probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// probeReadable 通过尝试列目录确认可读（空目录返回 EOF 属正常）。
func probeReadable(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Readdirnames(1); err != nil && err != io.EOF {
		return err
	}
	return nil
}

// ensureOutputDir 在保存设置时预先创建输出目录。
func ensureOutputDir(s config.Settings) error {
	if err := os.MkdirAll(s.OutputDir, 0o755); err != nil {
		return fmt.Errorf("输出目录创建失败 %s: %w", s.OutputDir, err)
	}
	return nil
}

// checkSourceDir 校验源目录可用。源目录**不自动创建**：
// 自动建空目录会让「挂载忘了加」表现为「扫描成功但 0 个文件」，比直接报错难排查得多。
func checkSourceDir(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("源目录不存在: %s（程序不会自动创建源目录，请检查挂载路径或 I2M_SOURCE_DIR）", dir)
		}
		return fmt.Errorf("源目录无法访问: %s: %w", dir, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("源路径不是目录: %s", dir)
	}
	return nil
}
