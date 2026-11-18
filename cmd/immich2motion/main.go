// immich2motion 把 Immich 图库中的苹果 Live Photo（HEIC+MOV）转换为
// Google 兼容的 Motion Photo（JPG 容器 + 尾部拼接 MOV + XMP MicroVideo 元数据）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/immich2motion/immich2motion/internal/config"
	"github.com/immich2motion/immich2motion/internal/engine"
	"github.com/immich2motion/immich2motion/internal/store"
	"github.com/immich2motion/immich2motion/internal/web"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "错误: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	var (
		webMode   = flag.Bool("web", false, "启动 Web 管理面板（默认行为）")
		scanOnce  = flag.Bool("scan-once", false, "只执行一次扫描并处理完队列后退出")
		dryRun    = flag.Bool("dry-run", false, "只扫描配对，不执行任何转换")
		showVer   = flag.Bool("version", false, "打印版本号并退出")
		healthChk = flag.Bool("health-check", false, "内部使用：探测本地 Web 服务健康状态（容器 HEALTHCHECK）")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "immich2motion - Live Photo → Google Motion Photo\n\n")
		fmt.Fprintf(os.Stderr, "用法:\n  immich2motion [选项]\n\n选项:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, `
环境变量:
  I2M_SOURCE_DIR      源目录（默认 /source，只读挂载）
  I2M_OUTPUT_DIR      输出目录（默认 /output）
  I2M_DATA_DIR        数据目录（默认 /data，存 SQLite）
  I2M_WEB_ADDR        Web 监听地址（默认 :8080）
  I2M_LOG_LEVEL       日志级别 info|warn|error（默认 info）
  I2M_WORKERS         并发数（默认 2）
  I2M_JPG_QUALITY     JPG 质量（默认 90）
  I2M_ENABLE_MD5      是否启用 MD5 校验（默认 true）
  I2M_AUTO_SCAN       是否自动持续扫描（默认 true）
  I2M_OUTPUT_SUFFIX   输出文件名后缀（默认 -motion）
  I2M_INCLUDE_UNCONVERTED
                      完整图库模式（默认 false）：把未参与转换的源文件原样复制到输出目录，
                      保持与源目录相同的相对路径，使同步出去的图库完整且不重复
  I2M_EXIFTOOL        exiftool 可执行文件
  I2M_HEIF_CONVERT    heif-convert 可执行文件
`)
	}
	flag.Parse()

	if *showVer {
		fmt.Printf("immich2motion %s\n", config.Version)
		return nil
	}
	if *healthChk {
		cfg := config.FromEnv()
		return probeHealth(cfg.WebAddr)
	}
	if *webMode && *scanOnce {
		return errors.New("--web 与 --scan-once 不能同时使用")
	}

	cfg := config.FromEnv()
	if err := cfg.EnsureDataDir(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	settings, err := st.EnsureSettings(ctx, config.NewSettingsFromEnv())
	if err != nil {
		return err
	}
	// 目录配置有问题不阻止启动：Web 面板需要能打开以便修正设置。
	if err := settings.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "警告: 目录配置存在问题，请在 Web 设置页修正: "+err.Error())
	}

	if *scanOnce || *dryRun {
		return runOnce(ctx, cfg, st, settings, *dryRun)
	}
	return runWeb(ctx, cfg, st, settings)
}

// runOnce CLI 单次执行：扫描 → （可选）处理队列 → 输出统计 → 退出。
func runOnce(ctx context.Context, cfg config.Config, st *store.Store, settings config.Settings, dry bool) error {
	settings.AutoScan = false // CLI 模式不做周期扫描
	fmt.Printf("配置：源目录=%s（只读） 输出目录=%s 数据目录=%s\n",
		settings.SourceDir, settings.OutputDir, cfg.DataDir)

	eng := engine.New(cfg, st, settings)

	if dry {
		eng.SetDryRun(true)
	} else {
		if err := eng.Start(ctx, false); err != nil {
			return err
		}
		defer eng.Stop()
	}

	res, err := eng.ScanNow(ctx, "cli")
	if err != nil {
		return err
	}

	fmt.Printf("扫描完成：文件 %d 个（HEIC %d / 视频 %d），耗时 %s\n",
		res.FilesSeen, res.HeicSeen, res.VideoSeen, res.Duration.Round(time.Millisecond))
	fmt.Printf("配对 %d 组（新增 %d / 变化 %d），未配对 %d 个，告警 %d 条\n",
		res.PairsTotal, res.PairsNew, res.PairsChanged, res.PairsUnpaired, res.Warnings)

	if dry {
		fmt.Println("Dry-Run：未执行任何转换，任务保持待处理状态")
		return nil
	}

	fmt.Println("开始处理队列…")
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		d, derr := eng.Dashboard(ctx)
		runnable, jerr := st.HasRunnableJob(ctx)
		if derr == nil && jerr == nil && !runnable && len(d.Running) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}

	d, err := eng.Dashboard(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("处理结束：成功 %d，失败 %d，跳过 %d，产物 %d 个\n",
		d.Jobs.Success, d.Jobs.Failed, d.Jobs.Skipped, d.Outputs)

	// 完整图库模式：队列跑完再镜像一次，这样「转换失败」的文件也会被原样送出，
	// 输出目录始终是一份完整、不重复的图库。
	if eng.Settings().IncludeUnconverted {
		fmt.Println("同步未转换的源文件到输出目录…")
		stats, merr := eng.MirrorNow(ctx)
		if merr != nil {
			fmt.Fprintf(os.Stderr, "警告: 同步未转换文件失败: %v\n", merr)
		} else {
			fmt.Printf("同步完成：遍历 %d 个，新复制 %d，更新 %d，跳过 %d，清理副本 %d，失败 %d\n",
				stats.Seen, stats.Copied, stats.Updated, stats.Skipped, stats.Pruned, stats.Failed)
		}
	}
	return nil
}

// runWeb 启动常驻 Web 服务。
func runWeb(ctx context.Context, cfg config.Config, st *store.Store, settings config.Settings) error {
	// 先把生效路径打出来：路径配错时这是最快能看出来的信息
	fmt.Printf("配置：源目录=%s（只读） 输出目录=%s 数据目录=%s\n",
		settings.SourceDir, settings.OutputDir, cfg.DataDir)

	eng := engine.New(cfg, st, settings)
	if err := eng.Start(ctx, true); err != nil {
		return err
	}
	defer eng.Stop()

	srv, err := web.NewServer(cfg, st, eng)
	if err != nil {
		return err
	}

	httpSrv := &http.Server{
		Addr:        cfg.WebAddr,
		Handler:     srv.Routes(),
		IdleTimeout: 120 * time.Second,
		// 不设置 WriteTimeout：SSE 是长连接，写超时会切断实时推送
	}

	errCh := make(chan error, 1)
	go func() {
		fmt.Printf("immich2motion %s 已启动，Web 面板: http://localhost%s\n", config.Version, portOnly(cfg.WebAddr))
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		fmt.Println("收到退出信号，正在关闭…")
	case err := <-errCh:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}

// portOnly 把 ":8080" 归一化成 ":8080"，"0.0.0.0:8080" 归一化成 ":8080"，
// 仅用于启动日志里的可点击链接。
func portOnly(addr string) string {
	if strings.HasPrefix(addr, "0.0.0.0:") {
		return addr[len("0.0.0.0"):]
	}
	if strings.HasPrefix(addr, "[::]:") {
		return addr[len("[::]"):]
	}
	return addr
}

// probeHealth 供容器 HEALTHCHECK 使用：请求本地 /healthz，非 200 则退出码非 0。
func probeHealth(addr string) error {
	host := addr
	switch {
	case strings.HasPrefix(addr, "0.0.0.0:"):
		host = "127.0.0.1:" + strings.TrimPrefix(addr, "0.0.0.0:")
	case strings.HasPrefix(addr, "[::]:"):
		host = "127.0.0.1:" + strings.TrimPrefix(addr, "[::]:")
	case strings.HasPrefix(addr, ":"):
		host = "127.0.0.1" + addr
	}
	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Get("http://" + host + "/healthz")
	if err != nil {
		return fmt.Errorf("健康检查失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("健康检查返回 %d", resp.StatusCode)
	}
	return nil
}
