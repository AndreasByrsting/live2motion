package engine

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/AndreasByrsting/live2motion/internal/library"
)

// ScanStatus 扫描的实时状态。
//
// 大图库首轮扫描可能要几分钟，期间配对/任务还没入库，仪表盘所有数字都是 0，
// 看起来像卡死。这里把「已经走过多少文件、正在做哪一步」暴露出去，
// 界面就能显示递增计数 + 旋转图标。
type ScanStatus struct {
	Active         bool   `json:"active"`
	Trigger        string `json:"trigger"`
	Phase          string `json:"phase"`
	FilesSeen      int64  `json:"files_seen"`
	HeicSeen       int64  `json:"heic_seen"`
	VideoSeen      int64  `json:"video_seen"`
	Done           int64  `json:"done"`
	Total          int64  `json:"total"`
	StartedAt      int64  `json:"started_at"`
	ElapsedSeconds int64  `json:"elapsed_seconds"`
}

// scanLiveState 扫描实时状态的并发容器。
type scanLiveState struct {
	mu   sync.RWMutex
	st   ScanStatus
	last atomic.Int64 // 上次广播时间（UnixNano），用于节流
}

// beginScanLive 标记一次扫描开始。
func (e *Engine) beginScanLive(trigger string) {
	e.scanLive.mu.Lock()
	e.scanLive.st = ScanStatus{
		Active:    true,
		Trigger:   trigger,
		Phase:     library.ScanPhaseWalk,
		StartedAt: time.Now().Unix(),
	}
	e.scanLive.mu.Unlock()
	e.scanLive.last.Store(0) // 首帧必须发出去
	e.publishScanLive(true)
	// publishScanLive 会把 last 设成当前时间；清零让第一次真正的进度回调
	// （带着文件计数）不被 400ms 节流吞掉。
	e.scanLive.last.Store(0)
}

// reportScanProgress 接收扫描器回调：更新状态并节流广播。
// 节流到约 2 次/秒——界面本来也是 600ms 合并刷新，再快只会白烧 CPU。
func (e *Engine) reportScanProgress(p library.ScanProgress) {
	changed := p.Phase != "" && e.phaseChanged(p.Phase)
	if !changed {
		if last := e.scanLive.last.Load(); last != 0 && time.Since(time.Unix(0, last)) < 400*time.Millisecond {
			return
		}
	}

	e.scanLive.mu.Lock()
	e.scanLive.st.Active = true
	if p.Phase != "" {
		e.scanLive.st.Phase = p.Phase
	}
	e.scanLive.st.FilesSeen = p.FilesSeen
	e.scanLive.st.HeicSeen = p.HeicSeen
	e.scanLive.st.VideoSeen = p.VideoSeen
	e.scanLive.st.Done = p.Done
	e.scanLive.st.Total = p.Total
	e.scanLive.st.ElapsedSeconds = int64(p.Elapsed.Seconds())
	e.scanLive.mu.Unlock()

	// 到这里说明已经通过了节流判断，直接发
	e.publishScanLive(true)
}

// phaseChanged 判断阶段是否发生变化（阶段切换要立即上报，不能被节流吞掉）。
func (e *Engine) phaseChanged(phase string) bool {
	e.scanLive.mu.RLock()
	defer e.scanLive.mu.RUnlock()
	return e.scanLive.st.Phase != phase
}

// finishScanLive 扫描结束（成功或失败）后清除实时状态。
func (e *Engine) finishScanLive() {
	e.scanLive.mu.Lock()
	e.scanLive.st.Active = false
	e.scanLive.st.Phase = ""
	e.scanLive.mu.Unlock()
	e.publishScanLive(true)
}

// ScanStatus 返回当前扫描状态快照。
func (e *Engine) ScanStatus() ScanStatus {
	e.scanLive.mu.RLock()
	defer e.scanLive.mu.RUnlock()
	return e.scanLive.st
}

func (e *Engine) publishScanLive(force bool) {
	now := time.Now().UnixNano()
	if !force {
		if last := e.scanLive.last.Load(); last != 0 && now-last < int64(400*time.Millisecond) {
			return
		}
	}
	e.scanLive.last.Store(now)

	d := e.ScanStatus()
	elapsed := int64(0)
	if d.StartedAt > 0 {
		elapsed = int64(time.Since(time.Unix(d.StartedAt, 0)).Seconds())
	}
	state := "done"
	if d.Active {
		state = "progress"
	}
	e.bus.Publish(Event{Type: EventScan, Data: map[string]any{
		"state":       state,
		"active":      d.Active,
		"trigger":     d.Trigger,
		"phase":       d.Phase,
		"files_seen":  d.FilesSeen,
		"heic_seen":   d.HeicSeen,
		"video_seen":  d.VideoSeen,
		"done":        d.Done,
		"total":       d.Total,
		"elapsed_sec": elapsed,
	}})
}
