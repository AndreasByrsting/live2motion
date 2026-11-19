package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AndreasByrsting/live2motion/internal/config"
	"github.com/AndreasByrsting/live2motion/internal/engine"
	"github.com/AndreasByrsting/live2motion/internal/store"
)

// 外部工具缺失时必须闭合闸门：worker 不去领任务，队列保持 pending。
// 否则每一轮重试都会把任务烧成 failed，等工具装好后用户还得手工重跑。
func TestQueueIsPreservedWhenToolsMissing(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "source")
	outDir := filepath.Join(dir, "output")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 直接构造一条已配对的任务，不需要外部工具参与
	heic := &store.Asset{
		SourceRoot: srcDir, RelPath: "a/IMG_0001.HEIC", Kind: store.KindHEIC,
		SizeBytes: 16, MtimeUnix: 1, State: store.AssetStateOK, LastScanID: 1,
	}
	video := &store.Asset{
		SourceRoot: srcDir, RelPath: "a/IMG_0001.MOV", Kind: store.KindVideo,
		SizeBytes: 512, MtimeUnix: 1, State: store.AssetStateOK, LastScanID: 1,
	}
	if _, err := st.UpsertAssets(ctx, []*store.Asset{heic, video}); err != nil {
		t.Fatal(err)
	}
	pair := &store.Pair{
		SourceRoot: srcDir, ContentID: "UUID-TEST",
		HeicAssetID: heic.ID, VideoAssetID: video.ID,
		HeicRelPath: heic.RelPath, VideoRelPath: video.RelPath,
		HeicMtime: 1, VideoMtime: 1, HeicSize: 16, VideoSize: 512,
	}
	pairID, _, _, err := st.UpsertPair(ctx, pair)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.EnsureJob(ctx, pairID, 2); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{
		DataDir:  dir,
		ExifBin:  "l2m-definitely-missing-exiftool",
		HeifBin:  "l2m-definitely-missing-heif",
		LogLevel: "error",
	}
	settings := config.DefaultSettings()
	settings.SourceDir = srcDir
	settings.OutputDir = outDir
	settings.AutoScan = false
	settings.ScanIntervalSec = 3600

	eng := engine.New(cfg, st, settings)
	if err := eng.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	defer eng.Stop()

	if eng.ToolsReady() {
		t.Fatal("外部工具缺失时 ToolsReady 应为 false")
	}
	rep := eng.Health()
	if rep.Ready {
		t.Fatalf("健康检查 Ready 应为 false: %+v", rep)
	}

	// 给 worker 充分的时间去「试图」领取任务
	time.Sleep(3 * time.Second)

	stats, err := st.JobStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending != 1 || stats.Running != 0 || stats.Failed != 0 {
		t.Fatalf("工具未就绪时不应消耗队列，实际 %+v", stats)
	}

	// 输出目录应该已被自动创建（程序自己的产出位置）
	if _, err := os.Stat(outDir); err != nil {
		t.Fatalf("输出目录未自动创建: %v", err)
	}
}
