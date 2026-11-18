package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/immich2motion/immich2motion/internal/library"
	"github.com/immich2motion/immich2motion/internal/webhook"
)

// 一轮「扫描 → 队列排空」的推送节奏。
//
// 早先的实现是每个任务成功/失败各推一条，几千张照片会把手机通知栏刷爆。
// 现在把一整轮扫描的结果汇总成一条：开始时一条、收尾时一条，
// 队列没有实际工作时（例如定时扫描什么都没发现）一条都不发。
const (
	// cycleIdleGrace 队列连续空闲这么久，认定这一轮结束。
	// worker 领任务的节奏是 2 秒一次，10 秒足够覆盖刚入队的任务。
	cycleIdleGrace = 10 * time.Second
	// cycleStallAfter 队列里还有任务但长时间毫无进展（暂停 / 工具不可用），
	// 到点先把这一轮结算掉，避免推送无限期挂着。
	cycleStallAfter = 20 * time.Minute
	// cycleMaxFailures 汇总里最多记录多少条失败明细（防止消息过长）。
	cycleMaxFailures = 50
)

// cycleState 一轮处理的累计统计。
type cycleState struct {
	startedAt  time.Time
	lastActive time.Time

	trigger  string
	scanDone bool
	started  bool // 是否已经推过「开始」通知
	finished bool

	files, heics, videos               int64
	pairsNew, pairsChanged, unpaired   int64
	success, failed, skipped, mirrored int64
	failures                           []string
	idleSince                          time.Time
}

// cycleTracker 保护当前这一轮统计。
type cycleTracker struct {
	mu  sync.Mutex
	cur *cycleState
}

// cycleLoop 周期检查队列是否已经排空，排空则结算本轮并推送。
func (e *Engine) cycleLoop(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.cycleTick(ctx)
		}
	}
}

func (e *Engine) cycleTick(ctx context.Context) {
	e.cycle.mu.Lock()
	c := e.cycle.cur
	e.cycle.mu.Unlock()
	if c == nil {
		return
	}

	// 扫描还没结束（或还没开始下一阶段）时，这一轮肯定没结束
	if e.scanning.Load() {
		return
	}
	// 完整图库模式的镜像复制发生在扫描之后，可能比扫描本身更久。
	// 等它跑完再结算，否则汇总里的「原样复制 N 个」会永远是 0。
	if e.mirroring.Load() {
		e.cycle.mu.Lock()
		c.idleSince = time.Time{}
		e.cycle.mu.Unlock()
		return
	}
	e.cycle.mu.Lock()
	scanDone := c.scanDone
	e.cycle.mu.Unlock()
	if !scanDone {
		return
	}

	js, err := e.st.JobStats(ctx)
	if err != nil {
		return
	}

	if js.Pending > 0 || js.Running > 0 {
		e.cycle.mu.Lock()
		c.idleSince = time.Time{}
		last := c.lastActive
		started := c.startedAt
		e.cycle.mu.Unlock()

		// 暂停/工具缺失会让队列一直不动，卡太久就先结算，别让推送无限期挂着
		ref := last
		if ref.IsZero() {
			ref = started
		}
		if time.Since(ref) >= cycleStallAfter {
			e.finishCycle(c, "队列长时间没有进展")
		}
		return
	}

	e.cycle.mu.Lock()
	if c.idleSince.IsZero() {
		c.idleSince = time.Now()
		e.cycle.mu.Unlock()
		return
	}
	idle := time.Since(c.idleSince)
	e.cycle.mu.Unlock()
	if idle < cycleIdleGrace {
		return
	}
	e.finishCycle(c, "队列已排空")
}

// beginCycle 新一轮扫描开始：开一个新周期，或者把本次扫描并入仍在收尾的周期。
//
// 为什么要并入：图库很大时一轮转换可能比扫描间隔更久，如果每次扫描都新开周期，
// 会把一整轮长任务切成一堆「0 成功」的碎片消息。
func (e *Engine) beginCycle(trigger string) {
	now := time.Now()
	e.cycle.mu.Lock()
	if c := e.cycle.cur; c != nil {
		// 上一轮还在跑：并进同一轮，只更新触发方式，不重复推「开始」
		c.scanDone = false
		c.trigger = trigger
		c.idleSince = time.Time{}
		e.cycle.mu.Unlock()
		return
	}
	c := &cycleState{startedAt: now, lastActive: now, trigger: trigger}
	e.cycle.cur = c
	e.cycle.mu.Unlock()

	// 手动/启动/深度校验：立即推一条「开始扫描」，让用户知道点下去有反应。
	// 定时扫描（auto）每 5 分钟一次，开始时推送纯属噪音，等真的发现新照片再说。
	if trigger == "auto" {
		return
	}
	e.cycle.mu.Lock()
	if e.cycle.cur == c {
		c.started = true
	}
	e.cycle.mu.Unlock()
	e.notifyCycle(webhook.KindStart, "开始扫描图库", fmt.Sprintf(
		"触发方式：%s\n源目录：%s", triggerText(trigger), e.Settings().SourceDir))
}

// cycleScanDone 扫描结束，记录本轮规模。
func (e *Engine) cycleScanDone(res *library.ScanResult) {
	if res == nil {
		return
	}
	e.cycle.mu.Lock()
	c := e.cycle.cur
	if c == nil {
		e.cycle.mu.Unlock()
		return
	}
	c.scanDone = true
	c.files, c.heics, c.videos = res.FilesSeen, res.HeicSeen, res.VideoSeen
	c.pairsNew += res.PairsNew
	c.pairsChanged += res.PairsChanged
	c.unpaired = res.PairsUnpaired
	c.lastActive = time.Now()
	needStart := !c.started && res.PairsNew > 0
	if needStart {
		c.started = true
	}
	trigger, pairsNew := c.trigger, res.PairsNew
	e.cycle.mu.Unlock()

	// 定时扫描发现了新照片：这时候推「开始」才有信息量
	if needStart {
		e.notifyCycle(webhook.KindStart, "开始转换实况照片", fmt.Sprintf(
			"扫描发现 %d 组新的实况照片（共 %d 个文件），已加入转换队列。",
			pairsNew, res.PairsNew*2))
		e.Info("webhook", "本轮发现 %d 组新配对，已推送开始通知（%s）", pairsNew, trigger)
	}
}

// cycleJobDone 记录一个任务成功。
func (e *Engine) cycleJobDone() {
	e.cycle.mu.Lock()
	if c := e.cycle.cur; c != nil {
		c.success++
		c.lastActive = time.Now()
	}
	e.cycle.mu.Unlock()
}

// cycleJobFailed 记录一个任务最终失败（重试耗尽或不可恢复）。
func (e *Engine) cycleJobFailed(relPath, reason string) {
	e.cycle.mu.Lock()
	if c := e.cycle.cur; c != nil {
		c.failed++
		c.lastActive = time.Now()
		if len(c.failures) < cycleMaxFailures {
			c.failures = append(c.failures, relPath+"："+shorten(reason, 120))
		}
	}
	e.cycle.mu.Unlock()
}

// cycleJobSkipped 记录一个被跳过的任务。
func (e *Engine) cycleJobSkipped() {
	e.cycle.mu.Lock()
	if c := e.cycle.cur; c != nil {
		c.skipped++
		c.lastActive = time.Now()
	}
	e.cycle.mu.Unlock()
}

// cycleMirrored 记录本轮原样复制出去的未转换文件数量。
func (e *Engine) cycleMirrored(n int64) {
	if n <= 0 {
		return
	}
	e.cycle.mu.Lock()
	if c := e.cycle.cur; c != nil {
		c.mirrored += n
		c.lastActive = time.Now()
	}
	e.cycle.mu.Unlock()
}

// finishCycle 结算一轮并推送汇总。没有实际工作时静默结束。
func (e *Engine) finishCycle(c *cycleState, reason string) {
	e.cycle.mu.Lock()
	if c.finished {
		e.cycle.mu.Unlock()
		return
	}
	c.finished = true
	if e.cycle.cur == c {
		e.cycle.cur = nil
	}
	e.cycle.mu.Unlock()

	work := c.pairsNew > 0 || c.pairsChanged > 0 ||
		c.success+c.failed+c.skipped > 0 || c.mirrored > 0
	if !work {
		// 定时扫描什么都没发现：不发消息，避免每 5 分钟骚扰一次
		return
	}

	elapsed := time.Since(c.startedAt)
	title := "本次处理完成"
	kind := webhook.KindSuccess
	if c.failed > 0 {
		kind = webhook.KindFailure
		title = fmt.Sprintf("本次处理完成（%d 个失败）", c.failed)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "扫描 %d 个文件（HEIC %d / 视频 %d），新增配对 %d 组",
		c.files, c.heics, c.videos, c.pairsNew)
	fmt.Fprintf(&b, "\n转换成功 %d，失败 %d", c.success, c.failed)
	if c.skipped > 0 {
		fmt.Fprintf(&b, "，跳过 %d", c.skipped)
	}
	if c.mirrored > 0 {
		fmt.Fprintf(&b, "\n原样复制未转换文件 %d 个", c.mirrored)
	}
	if c.unpaired > 0 {
		fmt.Fprintf(&b, "\n未配对文件 %d 个（不会生成任务）", c.unpaired)
	}
	fmt.Fprintf(&b, "\n耗时 %s", humanDuration(elapsed))

	if len(c.failures) > 0 {
		max := e.Settings().WebhookMaxItems
		if max < 1 {
			max = 10
		}
		b.WriteString("\n\n失败明细：")
		for i, f := range c.failures {
			if i >= max {
				fmt.Fprintf(&b, "\n… 还有 %d 条", len(c.failures)-max)
				break
			}
			b.WriteString("\n· " + f)
		}
	}
	if reason != "队列已排空" {
		b.WriteString("\n（" + reason + "）")
	}

	// 失败推送关掉、但完成推送开着时，仍然发一条（类型按完成算），
	// 免得用户明明想看「跑完了没」却因为失败开关关着收不到任何消息。
	cfg := e.Settings()
	if !webhook.Allowed(cfg, kind) && kind == webhook.KindFailure && webhook.Allowed(cfg, webhook.KindSuccess) {
		kind = webhook.KindSuccess
	}
	ev := webhook.Event{Kind: kind, Title: title, Detail: b.String()}
	if !webhook.Allowed(cfg, kind) {
		return
	}
	e.notifier.Notify(ev)
	e.Info("webhook", "本轮处理结束（%s）：成功 %d、失败 %d、耗时 %s，已推送汇总",
		reason, c.success, c.failed, humanDuration(elapsed))
}

// notifyCycle 发送一条周期级事件（不经过任何合并，直接推送）。
func (e *Engine) notifyCycle(kind webhook.Kind, title, detail string) {
	cfg := e.Settings()
	if !webhook.Allowed(cfg, kind) {
		return
	}
	e.notifier.Notify(webhook.Event{Kind: kind, Title: title, Detail: detail})
}

// triggerText 触发方式的中文描述。
func triggerText(trigger string) string {
	switch trigger {
	case "manual":
		return "手动触发"
	case "startup":
		return "服务启动"
	case "auto":
		return "定时扫描"
	case "deep-verify":
		return "深度 MD5 校验"
	case "dry-run":
		return "Dry-Run"
	case "enable":
		return "开启完整图库模式"
	case "cli":
		return "命令行"
	default:
		return trigger
	}
}

// humanDuration 把耗时格式化成「X 分 Y 秒」这样的短文本。
func humanDuration(d time.Duration) string {
	if d < time.Second {
		return "不到 1 秒"
	}
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%d 分 %d 秒", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%d 小时 %d 分", int(d.Hours()), int(d.Minutes())%60)
}

// shorten 截断过长的错误信息，避免单条明细占满整条推送。
func shorten(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
