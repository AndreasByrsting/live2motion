package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/immich2motion/immich2motion/internal/config"
	"github.com/immich2motion/immich2motion/internal/convert"
	"github.com/immich2motion/immich2motion/internal/library"
	"github.com/immich2motion/immich2motion/internal/store"
	"github.com/immich2motion/immich2motion/internal/webhook"
)

// maxWorkerGoroutines worker 池的固定 goroutine 数量，
// 真正的并发由 dynSem 的 limit 控制（等于设置里的并发数）。
const maxWorkerGoroutines = 16

// ProgressEvent 单个任务的实时进度，推给前端。
type ProgressEvent struct {
	JobID       int64   `json:"job_id"`
	PairID      int64   `json:"pair_id"`
	ContentID   string  `json:"content_id"`
	HeicRelPath string  `json:"heic_rel_path"`
	Stage       string  `json:"stage"`
	Percent     float64 `json:"percent"`
	UpdatedAt   int64   `json:"updated_at"`
}

// Dashboard 仪表盘聚合数据。
type Dashboard struct {
	Jobs          store.JobStats      `json:"jobs"`
	PairsTotal    int64               `json:"pairs_total"`
	PairsUnpaired store.UnpairedCount `json:"pairs_unpaired"`
	Outputs       int64               `json:"outputs"`
	LastScan      *store.ScanRun      `json:"last_scan"`
	Running       []ProgressEvent     `json:"running"`
	Settings      config.Settings     `json:"settings"`
	Paused        bool                `json:"paused"`
	DryRun        bool                `json:"dry_run"`
	Scanning      bool                `json:"scanning"`
	UptimeSeconds int64               `json:"uptime_seconds"`
	SpeedMBps     float64             `json:"speed_mbps"`
	ETASeconds    int64               `json:"eta_seconds"`
	QueueDepth    int64               `json:"queue_depth"`
	ActiveWorkers int                 `json:"active_workers"`
	BitrotCount   int64               `json:"bitrot_count"`
	Health        HealthReport        `json:"health"`
}

// Engine 调度中心：扫描循环 + 任务队列 + worker 池 + 事件广播。
type Engine struct {
	cfg config.Config
	st  *store.Store
	bus *EventBus

	scanner  *library.Scanner
	pipeline *convert.Pipeline
	notifier *webhook.Notifier

	settingsMu sync.RWMutex
	settings   config.Settings

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	scanReq chan string
	wakeMu  sync.Mutex
	wake    chan struct{}

	scanning  atomic.Bool
	paused    atomic.Bool
	dryRun    atomic.Bool
	forceDeep atomic.Bool

	// 环境自检结果与「能否转换」闸门
	healthMu   sync.RWMutex
	health     HealthReport
	toolsReady atomic.Bool

	sem *dynSem

	currentMu sync.RWMutex
	current   map[int64]ProgressEvent

	thr       throughput
	startedAt time.Time
	lastScan  atomic.Int64
}

// New 构造引擎。settings 需已从数据库加载。
func New(cfg config.Config, st *store.Store, settings config.Settings) *Engine {
	e := &Engine{
		cfg:      cfg,
		st:       st,
		bus:      NewEventBus(),
		pipeline: convert.NewPipeline(cfg.HeifBin, cfg.ExifBin),
		settings: settings,
		scanReq:  make(chan string, 1),
		wake:     make(chan struct{}),
		current:  make(map[int64]ProgressEvent),
	}
	e.scanner = library.NewScanner(st, cfg.ExifBin, e)
	e.notifier = webhook.New(settings, func(level, format string, args ...any) {
		e.logf(level, "webhook", format, args...)
	})
	e.paused.Store(settings.Paused)
	e.sem = newDynSem(settings.Workers)
	return e
}

// Bus 暴露事件总线给 Web 层订阅。
func (e *Engine) Bus() *EventBus { return e.bus }

// Start 启动扫描循环与 worker 池。initialScan=false 时不执行启动扫描（CLI 单次模式用）。
func (e *Engine) Start(parent context.Context, initialScan bool) error {
	e.ctx, e.cancel = context.WithCancel(parent)
	e.startedAt = time.Now()

	// 崩溃恢复：上次进程被杀时残留的 running 任务打回 pending
	if n, err := e.st.RecoverRunning(e.ctx); err != nil {
		e.Error("engine", "恢复中断任务失败: %v", err)
	} else if n > 0 {
		e.Warn("engine", "上次退出时有 %d 个任务处于处理中，已重新排队", n)
	}

	// 环境自检：目录状态 + 外部工具。工具缺失时闭合闸门，worker 不会去烧任务队列。
	e.refreshHealth(e.ctx)

	e.Info("engine", "服务启动：并发 %d，质量 %d，MD5 %v，扫描间隔 %ds",
		e.Settings().Workers, e.Settings().JPGQuality, e.Settings().EnableMD5, e.Settings().ScanIntervalSec)

	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		e.scanLoop(e.ctx, initialScan)
	}()

	for i := 0; i < maxWorkerGoroutines; i++ {
		e.wg.Add(1)
		go func(id int) {
			defer e.wg.Done()
			e.workerLoop(e.ctx, id)
		}(i)
	}
	return nil
}

// Stop 停止所有后台任务。
func (e *Engine) Stop() {
	if e.cancel != nil {
		e.cancel()
	}
	e.wg.Wait()
	if e.notifier != nil {
		e.notifier.Close()
	}
}

// scanLoop 周期扫描，同时响应手动触发。
func (e *Engine) scanLoop(ctx context.Context, initialScan bool) {
	// 启动即扫描一次，避免容器重启后长时间空转
	if initialScan {
		e.runScan(ctx, "startup")
	}

	for {
		interval := time.Duration(e.Settings().ScanIntervalSec) * time.Second
		if interval < 10*time.Second {
			interval = 10 * time.Second
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case trig := <-e.scanReq:
			timer.Stop()
			e.runScan(ctx, trig)
		case <-timer.C:
			if !e.Settings().AutoScan {
				continue
			}
			e.runScan(ctx, "auto")
		}
	}
}

// runScan 执行一次扫描，扫描期间不重复触发。
func (e *Engine) runScan(ctx context.Context, trigger string) {
	if !e.scanning.CompareAndSwap(false, true) {
		e.Warn("scan", "已有扫描在进行中，跳过本次触发(%s)", trigger)
		return
	}
	defer e.scanning.Store(false)

	st := e.Settings()
	if err := st.Validate(); err != nil {
		e.Error("scan", "目录配置无效: %v", err)
		return
	}

	e.bus.Publish(Event{Type: EventScan, Data: map[string]any{"state": "start", "trigger": trigger}})
	e.Info("scan", "开始扫描 %s（%s）", st.SourceDir, trigger)

	res, err := e.scanner.Scan(ctx, library.ScanOptions{
		SourceRoot:      st.SourceDir,
		Settings:        st,
		Trigger:         trigger,
		ForceDeepVerify: e.forceDeep.Swap(false),
	})
	if err != nil {
		e.Error("scan", "扫描失败: %v", err)
		e.bus.Publish(Event{Type: EventScan, Data: map[string]any{"state": "error", "error": err.Error()}})
		e.notifier.Notify(webhook.Event{
			Kind: webhook.KindSystem, Title: "扫描失败",
			Detail: fmt.Sprintf("扫描 %s 失败：%v", st.SourceDir, err),
		})
		return
	}

	e.lastScan.Store(time.Now().Unix())
	e.Info("scan", "扫描完成：文件 %d（HEIC %d / 视频 %d），配对 %d 组（新增 %d、变化 %d），未配对 %d，告警 %d，耗时 %s",
		res.FilesSeen, res.HeicSeen, res.VideoSeen, res.PairsTotal, res.PairsNew, res.PairsChanged,
		res.PairsUnpaired, res.Warnings, res.Duration.Round(time.Millisecond))

	// 顺带清理过期日志，避免 SQLite 无限增长
	if err := e.st.PruneLogs(ctx, st.LogRetention); err != nil {
		e.Warn("scan", "清理历史日志失败: %v", err)
	}

	e.wakeAll()
	e.bus.Publish(Event{Type: EventScan, Data: map[string]any{"state": "done"}})
	e.publishStats()
}

// workerLoop 常驻 worker：先抢并发名额，再原子领取任务。
func (e *Engine) workerLoop(ctx context.Context, id int) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if e.paused.Load() || e.dryRun.Load() || !e.toolsReady.Load() {
			// dry-run：队列保持待处理；工具未就绪：不领任务，避免把整队烧成 failed
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}

		if !e.sem.Acquire(ctx) {
			return
		}

		job, err := e.st.ClaimNextJob(ctx)
		if err != nil {
			e.sem.Release()
			if ctx.Err() != nil {
				return
			}
			e.Error("worker", "领取任务失败: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}
		if job == nil {
			e.sem.Release()
			ch := e.wakeChan()
			select {
			case <-ctx.Done():
				return
			case <-ch:
			case <-time.After(2 * time.Second):
			}
			continue
		}

		e.runJob(ctx, job)
		e.sem.Release()
		e.wakeAll()
	}
}

// runJob 执行单个转换任务。
func (e *Engine) runJob(ctx context.Context, job *store.JobView) {
	began := time.Now()
	pair, err := e.st.GetPair(ctx, job.PairID)
	if err != nil {
		e.Error("worker", "读取配对 %d 失败: %v", job.PairID, err)
		_ = e.st.FailJobPermanent(ctx, job.JobID, "配对记录不存在: "+err.Error())
		return
	}

	if e.dryRun.Load() {
		if err := e.st.SkipJob(ctx, job.JobID, "dry-run：已确认配对，未执行转换"); err != nil {
			e.Warn("dry-run", "更新任务状态失败: %v", err)
		}
		e.Info("dry-run", "可转换: %s + %s", pair.HeicRelPath, pair.VideoRelPath)
		e.publishStats()
		return
	}

	st := e.Settings()
	e.setCurrent(job, pair, "queued", 0)
	defer e.clearCurrent(job.JobID)

	e.Info("convert", "开始转换 %s", pair.HeicRelPath)
	e.notifier.Notify(webhook.Event{
		Kind:   webhook.KindStart,
		Title:  pair.HeicRelPath,
		Detail: "开始转换 " + pair.HeicRelPath,
	})

	if st.VerifyBeforeConvert && st.EnableMD5 {
		if err := e.preflightIntegrity(ctx, st, pair); err != nil {
			e.Error("convert", "拒绝转换 %s: %v", pair.HeicRelPath, err)
			_ = e.st.FailJobPermanent(ctx, job.JobID, err.Error())
			e.publishStats()
			return
		}
	}

	var lastPct float64
	res, err := e.pipeline.Execute(ctx, convert.Job{
		JobID:    job.JobID,
		Pair:     pair,
		Settings: st,
		Progress: func(stage string, pct float64) {
			if pct < 1 && pct-lastPct < 0.02 {
				return
			}
			lastPct = pct
			_ = e.st.UpdateProgress(ctx, job.JobID, stage, pct)
			e.setCurrent(job, pair, stage, pct)
		},
	})
	if err != nil {
		e.handleFailure(ctx, job, pair, err)
		return
	}

	if err := e.st.SaveOutput(ctx, &store.Output{
		PairID:      pair.ID,
		JobID:       job.JobID,
		SourceRoot:  pair.SourceRoot,
		RelPath:     res.RelPath,
		AbsPath:     res.AbsPath,
		SizeBytes:   res.SizeBytes,
		JPGBytes:    res.JPGBytes,
		VideoBytes:  res.VideoBytes,
		MD5:         res.MD5,
		XMPVerified: true,
	}); err != nil {
		e.Warn("convert", "保存产物记录失败: %v", err)
	}
	if err := e.st.CompleteJob(ctx, job.JobID); err != nil {
		e.Error("convert", "更新任务状态失败: %v", err)
	}

	e.thr.add(res.SizeBytes)
	e.Info("convert", "完成 %s（%.1f MB，耗时 %s）",
		res.RelPath, float64(res.SizeBytes)/(1024*1024), time.Since(began).Round(time.Millisecond))
	e.notifier.Notify(webhook.Event{
		Kind:  webhook.KindSuccess,
		Title: res.RelPath,
		Detail: fmt.Sprintf("%s（%.1f MB，耗时 %s）",
			res.RelPath, float64(res.SizeBytes)/(1024*1024), time.Since(began).Round(time.Millisecond)),
	})
	e.publishStats()
}

// handleFailure 区分可重试与不可恢复错误。
func (e *Engine) handleFailure(ctx context.Context, job *store.JobView, pair store.Pair, err error) {
	msg := err.Error()
	permanent := strings.Contains(msg, "源文件已不存在") ||
		strings.Contains(msg, "拒绝写入源目录") ||
		strings.Contains(msg, "不在 输出目录")

	if permanent {
		_ = e.st.FailJobPermanent(ctx, job.JobID, msg)
		e.Error("convert", "失败（不重试）%s: %s", pair.HeicRelPath, msg)
		e.notifier.Notify(webhook.Event{
			Kind: webhook.KindFailure, Title: pair.HeicRelPath,
			Detail: "不再重试：" + msg,
		})
		e.publishStats()
		return
	}

	final, ferr := e.st.FailJob(ctx, job.JobID, msg)
	if ferr != nil {
		e.Error("convert", "记录失败状态出错: %v", ferr)
	}
	if final {
		e.Error("convert", "失败（重试已耗尽）%s: %s", pair.HeicRelPath, msg)
		e.notifier.Notify(webhook.Event{
			Kind: webhook.KindFailure, Title: pair.HeicRelPath,
			Detail: "重试已耗尽：" + msg,
		})
	} else {
		e.Warn("convert", "失败（稍后重试）%s: %s", pair.HeicRelPath, msg)
	}
	e.publishStats()
}

// preflightIntegrity 转换前校验源文件 MD5，拦截静默损坏的源文件。
func (e *Engine) preflightIntegrity(ctx context.Context, st config.Settings, pair store.Pair) error {
	items := []struct {
		assetID int64
		rel     string
	}{
		{pair.HeicAssetID, pair.HeicRelPath},
		{pair.VideoAssetID, pair.VideoRelPath},
	}
	for _, it := range items {
		asset, err := e.st.GetAsset(ctx, it.assetID)
		if err != nil || asset.MD5 == "" {
			continue
		}
		if asset.MD5MtimeUnix != asset.MtimeUnix {
			continue // MD5 记录已过期（文件被替换），交给扫描流程重算
		}
		abs := filepath.Join(st.SourceDir, filepath.FromSlash(it.rel))
		ok, err := library.VerifyMD5(abs, asset.MD5)
		if err != nil {
			return fmt.Errorf("校验 %s 失败: %w", it.rel, err)
		}
		if !ok {
			return fmt.Errorf("源文件 MD5 与记录不一致（疑似静默损坏或写入未完成）: %s", it.rel)
		}
	}
	return nil
}

// ---------- 对外操作 ----------

// Settings 返回当前设置快照。
func (e *Engine) Settings() config.Settings {
	e.settingsMu.RLock()
	defer e.settingsMu.RUnlock()
	return e.settings
}

// UpdateSettings 保存并立即应用新设置。
func (e *Engine) UpdateSettings(ctx context.Context, s config.Settings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	s.Normalize()
	prev := e.Settings()

	// 只有源目录变了才强制校验，避免「改并发数」被无关的路径问题卡住
	if s.SourceDir != prev.SourceDir {
		if err := checkSourceDir(s.SourceDir); err != nil {
			return err
		}
	}
	// 输出目录属于程序自己的产出位置，不存在就自动创建
	if err := ensureOutputDir(s); err != nil {
		return err
	}

	if err := e.st.SaveSettings(ctx, s); err != nil {
		return err
	}
	e.settingsMu.Lock()
	e.settings = s
	e.settingsMu.Unlock()

	e.sem.SetLimit(s.Workers)
	e.paused.Store(s.Paused)
	if e.notifier != nil {
		e.notifier.Update(s)
	}
	e.Info("settings", "设置已更新：并发 %d，质量 %d，MD5 %v，扫描间隔 %ds，自动扫描 %v",
		s.Workers, s.JPGQuality, s.EnableMD5, s.ScanIntervalSec, s.AutoScan)

	// 目录可能刚被创建/修正，重新自检一次并唤醒 worker
	e.refreshHealth(ctx)
	e.wakeAll()
	e.publishStats()
	return nil
}

// TriggerScan 异步触发一次扫描，返回是否成功入队。
func (e *Engine) TriggerScan(trigger string) bool {
	select {
	case e.scanReq <- trigger:
		return true
	default:
		return false
	}
}

// TriggerDeepVerify 触发一次深度 MD5 校验：忽略 mtime 强制重算，
// 用于发现「mtime 没变但内容变了」的静默损坏（bit rot）。
func (e *Engine) TriggerDeepVerify() bool {
	e.forceDeep.Store(true)
	if !e.TriggerScan("deep-verify") {
		e.forceDeep.Store(false)
		return false
	}
	e.Info("md5", "已触发深度校验：将忽略 mtime 重算所有文件 MD5")
	return true
}

// SendTestWebhook 立即推送一条测试消息（不走合并窗口），返回错误供前端提示。
func (e *Engine) SendTestWebhook() error {
	if e.notifier == nil {
		return fmt.Errorf("通知模块未初始化")
	}
	return e.notifier.SendNow(webhook.Event{
		Kind:   webhook.KindTest,
		Title:  "测试消息",
		Detail: "这是一条来自 immich2motion 的测试推送，收到即表示配置正确。",
	})
}

// ScanNow 同步执行一次扫描（CLI 模式使用）。
func (e *Engine) ScanNow(ctx context.Context, trigger string) (*library.ScanResult, error) {
	st := e.Settings()
	if err := st.Validate(); err != nil {
		return nil, err
	}
	return e.scanner.Scan(ctx, library.ScanOptions{
		SourceRoot: st.SourceDir,
		Settings:   st,
		Trigger:    trigger,
	})
}

// SetPaused 暂停/恢复任务执行。
func (e *Engine) SetPaused(p bool) {
	e.paused.Store(p)
	if p {
		e.Info("engine", "任务已暂停")
	} else {
		e.Info("engine", "任务已恢复")
		e.wakeAll()
	}
	e.publishStats()
}

// Paused 是否暂停。
func (e *Engine) Paused() bool { return e.paused.Load() }

// SetDryRun 设置 dry-run 状态。
func (e *Engine) SetDryRun(v bool) {
	e.dryRun.Store(v)
	if v {
		e.Info("engine", "已开启 Dry-Run：只扫描配对，不执行转换")
	} else {
		e.Info("engine", "已关闭 Dry-Run")
	}
	e.publishStats()
}

// DryRun 是否处于 dry-run。
func (e *Engine) DryRun() bool { return e.dryRun.Load() }

// Wake 唤醒 worker（手工重置任务后调用）。
func (e *Engine) Wake() { e.wakeAll() }

// RunQueue 立即尝试执行队列（不等下一个扫描周期）。
func (e *Engine) RunQueue() { e.wakeAll() }

// Dashboard 聚合首页数据。
func (e *Engine) Dashboard(ctx context.Context) (Dashboard, error) {
	st := e.Settings()
	d := Dashboard{
		Settings: st,
		Paused:   e.paused.Load(),
		DryRun:   e.dryRun.Load(),
		Scanning: e.scanning.Load(),
		Running:  e.RunningJobs(),
	}
	if !e.startedAt.IsZero() {
		d.UptimeSeconds = int64(time.Since(e.startedAt).Seconds())
	}
	js, err := e.st.JobStats(ctx)
	if err == nil {
		d.Jobs = js
		d.QueueDepth = js.Pending
	}
	if n, err := e.st.CountPairs(ctx, st.SourceDir); err == nil {
		d.PairsTotal = n
	}
	if u, err := e.st.CountUnpaired(ctx, st.SourceDir); err == nil {
		d.PairsUnpaired = u
	}
	if n, err := e.st.CountOutputs(ctx, st.SourceDir); err == nil {
		d.Outputs = n
	}
	if ls, err := e.st.LastScan(ctx); err == nil {
		d.LastScan = ls
	}
	d.ActiveWorkers = e.sem.Active()
	d.Health = e.Health()

	avg, bps := e.thr.snapshot()
	d.SpeedMBps = bps / (1024 * 1024)
	if avg > 0 && js.Pending > 0 {
		workers := st.Workers
		if workers < 1 {
			workers = 1
		}
		d.ETASeconds = int64(float64(js.Pending) * avg / float64(workers))
	}
	return d, nil
}

// RunningJobs 当前正在转换的任务快照。
func (e *Engine) RunningJobs() []ProgressEvent {
	e.currentMu.RLock()
	defer e.currentMu.RUnlock()
	out := make([]ProgressEvent, 0, len(e.current))
	for _, v := range e.current {
		out = append(out, v)
	}
	return out
}

func (e *Engine) setCurrent(job *store.JobView, pair store.Pair, stage string, pct float64) {
	ev := ProgressEvent{
		JobID: job.JobID, PairID: job.PairID, ContentID: pair.ContentID,
		HeicRelPath: pair.HeicRelPath, Stage: stage, Percent: pct,
		UpdatedAt: time.Now().Unix(),
	}
	e.currentMu.Lock()
	e.current[job.JobID] = ev
	e.currentMu.Unlock()
	e.bus.Publish(Event{Type: EventJob, Data: ev})
}

func (e *Engine) clearCurrent(jobID int64) {
	e.currentMu.Lock()
	delete(e.current, jobID)
	e.currentMu.Unlock()
	e.bus.Publish(Event{Type: EventJob, Data: map[string]any{"job_id": jobID, "stage": "finished"}})
}

func (e *Engine) wakeChan() <-chan struct{} {
	e.wakeMu.Lock()
	defer e.wakeMu.Unlock()
	return e.wake
}

// wakeAll 广播唤醒所有等待中的 worker。
func (e *Engine) wakeAll() {
	e.wakeMu.Lock()
	close(e.wake)
	e.wake = make(chan struct{})
	e.wakeMu.Unlock()
	e.bus.Publish(Event{Type: EventStats, Data: nil})
}

func (e *Engine) publishStats() {
	e.bus.Publish(Event{Type: EventStats, Data: nil})
}

// ---------- library.Logger 实现 ----------

// Info 输出 info 日志。
func (e *Engine) Info(scope, format string, args ...any) {
	e.logf(store.LevelInfo, scope, format, args...)
}

// Warn 输出 warn 日志。
func (e *Engine) Warn(scope, format string, args ...any) {
	e.logf(store.LevelWarn, scope, format, args...)
}

// Error 输出 error 日志。
func (e *Engine) Error(scope, format string, args ...any) {
	e.logf(store.LevelError, scope, format, args...)
}

func (e *Engine) logf(level, scope, format string, args ...any) {
	if store.LogLevelRank(level) < store.LogLevelRank(e.cfg.LogLevel) {
		return
	}
	msg := fmt.Sprintf(format, args...)
	entry := store.LogEntry{
		TS:      time.Now().Unix(),
		Level:   level,
		Scope:   scope,
		Message: msg,
	}
	e.bus.Publish(Event{Type: EventLog, Data: entry})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := e.st.AppendLog(ctx, level, scope, msg); err != nil {
		// 日志写库失败不能再写日志，避免递归
		_ = err
	}
}
