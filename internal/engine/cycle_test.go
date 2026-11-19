package engine

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndreasByrsting/live2motion/internal/config"
	"github.com/AndreasByrsting/live2motion/internal/library"
	"github.com/AndreasByrsting/live2motion/internal/store"
)

// recvServer 起一个假接收端，记录收到的所有推送报文。
type recvServer struct {
	*httptest.Server
	mu   sync.Mutex
	msgs []string
}

func newRecvServer(t *testing.T) *recvServer {
	t.Helper()
	r := &recvServer{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.msgs = append(r.msgs, string(b))
		r.mu.Unlock()
		_, _ = w.Write([]byte(`{"errcode":0}`))
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *recvServer) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.msgs...)
}

func (r *recvServer) reset() {
	r.mu.Lock()
	r.msgs = nil
	r.mu.Unlock()
}

// waitFor 等待至少 n 条推送（推送是异步 goroutine 发出的）。
func (r *recvServer) waitFor(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if msgs := r.snapshot(); len(msgs) >= n {
			return msgs
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待 %d 条推送超时，实际收到 %d 条", n, len(r.snapshot()))
	return nil
}

func newCycleTestEngine(t *testing.T, webhookURL string) (*Engine, func()) {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	out := filepath.Join(dir, "output")
	for _, d := range []string{src, out} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}

	s := config.DefaultSettings()
	s.SourceDir = src
	s.OutputDir = out
	s.WebhookEnabled = webhookURL != ""
	s.WebhookURL = webhookURL
	s.WebhookOnStart = true
	s.WebhookOnSuccess = true
	s.WebhookOnFailure = true
	s.WebhookOnSystem = true
	s.WebhookMaxItems = 3

	e := New(config.Config{DataDir: dir, LogLevel: "error"}, st, s)
	return e, func() {
		e.notifier.Close()
		st.Close()
	}
}

func currentCycle(t *testing.T, e *Engine) *cycleState {
	t.Helper()
	e.cycle.mu.Lock()
	defer e.cycle.mu.Unlock()
	if e.cycle.cur == nil {
		t.Fatal("当前没有进行中的轮次")
	}
	return e.cycle.cur
}

// findMsg 在收到的报文里找包含指定片段的一条（推送是异步的，顺序不保证）。
func findMsg(msgs []string, substr string) string {
	for _, m := range msgs {
		if strings.Contains(m, substr) {
			return m
		}
	}
	return ""
}

// 一轮「扫描 → 队列排空」只推两条：开始一条、结束一条汇总。
// 逐任务推送是旧行为（几千张照片会刷爆通知栏），这里锁死新语义。
func TestCyclePushesStartAndSingleSummary(t *testing.T) {
	recv := newRecvServer(t)
	e, cleanup := newCycleTestEngine(t, recv.URL)
	defer cleanup()

	e.beginCycle("manual")
	e.cycleScanDone(&library.ScanResult{
		FilesSeen: 1284, HeicSeen: 642, VideoSeen: 642,
		PairsNew: 3, PairsUnpaired: 1,
	})
	e.cycleJobDone()
	e.cycleJobDone()
	e.cycleJobFailed("2024/05/IMG_5012.HEIC", "heif-convert 退出码 1")
	e.cycleJobSkipped()
	e.cycleMirrored(2)

	c := currentCycle(t, e)
	e.finishCycle(c, "队列已排空")

	msgs := recv.waitFor(t, 2)
	// 再等一会儿，确认没有第三条（逐任务推送的残留）
	time.Sleep(200 * time.Millisecond)
	msgs = recv.snapshot()
	if len(msgs) != 2 {
		t.Fatalf("一轮应只推 2 条，实际 %d 条：%v", len(msgs), msgs)
	}
	if findMsg(msgs, "开始扫描") == "" {
		t.Errorf("缺少开始通知：%v", msgs)
	}
	summary := findMsg(msgs, "本次处理完成")
	if summary == "" {
		t.Fatalf("缺少完成汇总：%v", msgs)
	}
	for _, want := range []string{"转换成功 2，失败 1", "原样复制未转换文件 2 个", "IMG_5012.HEIC", "耗时"} {
		if !strings.Contains(summary, want) {
			t.Errorf("汇总缺少 %q：%s", want, summary)
		}
	}
	// 失败明细最多列 WebhookMaxItems 条，这里只有 1 条，不该出现截断提示
	if strings.Contains(summary, "还有") {
		t.Errorf("失败明细不应被截断：%s", summary)
	}
}

// 定时扫描什么都没发现时一条都不发，否则每 5 分钟骚扰一次。
func TestCycleSilentWhenNoWork(t *testing.T) {
	recv := newRecvServer(t)
	e, cleanup := newCycleTestEngine(t, recv.URL)
	defer cleanup()

	e.beginCycle("auto") // 定时扫描不推「开始」
	e.cycleScanDone(&library.ScanResult{FilesSeen: 900, HeicSeen: 450, VideoSeen: 450})
	e.finishCycle(currentCycle(t, e), "队列已排空")

	time.Sleep(250 * time.Millisecond)
	if msgs := recv.snapshot(); len(msgs) != 0 {
		t.Fatalf("没有实际工作不应推送，实际推了 %d 条：%v", len(msgs), msgs)
	}
}

// 长图库的一轮转换可能跨过多次定时扫描：新扫描必须并入同一轮，
// 否则会被切成一堆「0 成功」的碎片消息。
func TestCycleMergesOverlappingScans(t *testing.T) {
	recv := newRecvServer(t)
	e, cleanup := newCycleTestEngine(t, recv.URL)
	defer cleanup()

	e.beginCycle("manual")
	first := currentCycle(t, e)
	e.cycleScanDone(&library.ScanResult{FilesSeen: 100, PairsNew: 2})

	e.beginCycle("auto") // 队列还没排空，又到了一次扫描
	second := currentCycle(t, e)
	if first != second {
		t.Fatal("长队列未排空时再次扫描应当并入同一轮")
	}
	e.cycleScanDone(&library.ScanResult{FilesSeen: 120, PairsNew: 1})
	e.cycleJobDone()

	e.finishCycle(second, "队列已排空")

	msgs := recv.waitFor(t, 2)
	summary := findMsg(msgs, "本次处理完成")
	if summary == "" {
		t.Fatalf("缺少完成汇总：%v", msgs)
	}
	if !strings.Contains(summary, "新增配对 3 组") {
		t.Errorf("两次扫描的新增配对应累加为 3 组：%s", summary)
	}
}

// 失败推送关掉、完成推送开着时，仍要收到一条汇总（类型降级为完成）。
func TestCycleFallsBackToSuccessKind(t *testing.T) {
	recv := newRecvServer(t)
	e, cleanup := newCycleTestEngine(t, recv.URL)
	defer cleanup()

	s := e.Settings()
	s.WebhookOnFailure = false
	if err := e.UpdateSettings(context.Background(), s); err != nil {
		t.Fatalf("更新设置失败: %v", err)
	}

	e.beginCycle("manual")
	e.cycleScanDone(&library.ScanResult{FilesSeen: 10, PairsNew: 1})
	e.cycleJobFailed("a/IMG_1.HEIC", "boom")
	e.finishCycle(currentCycle(t, e), "队列已排空")

	msgs := recv.waitFor(t, 2)
	if m := findMsg(msgs, "本次处理完成（1 个失败）"); m == "" {
		t.Errorf("应仍然收到带失败数量的汇总：%v", msgs)
	}
}

// 扫描进度：回调必须落到 ScanStatus 上，仪表盘才有数字可显示。
func TestScanProgressReported(t *testing.T) {
	e, cleanup := newCycleTestEngine(t, "")
	defer cleanup()

	e.beginScanLive("manual")
	e.reportScanProgress(library.ScanProgress{
		Phase: library.ScanPhaseWalk, FilesSeen: 321, HeicSeen: 200, VideoSeen: 121,
	})
	st := e.ScanStatus()
	if !st.Active || st.FilesSeen != 321 || st.HeicSeen != 200 || st.Phase != library.ScanPhaseWalk {
		t.Fatalf("扫描状态未更新：%+v", st)
	}

	e.reportScanProgress(library.ScanProgress{
		Phase: library.ScanPhaseMD5, FilesSeen: 321, Done: 40, Total: 300,
	})
	if st := e.ScanStatus(); st.Phase != library.ScanPhaseMD5 || st.Total != 300 || st.Done != 40 {
		t.Fatalf("阶段切换应保留文件计数并更新阶段：%+v", st)
	}

	e.finishScanLive()
	if st := e.ScanStatus(); st.Active {
		t.Fatalf("扫描结束后不应仍标记为进行中：%+v", st)
	}
}
