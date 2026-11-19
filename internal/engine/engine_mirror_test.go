package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/AndreasByrsting/live2motion/internal/config"
	"github.com/AndreasByrsting/live2motion/internal/engine"
	"github.com/AndreasByrsting/live2motion/internal/store"
)

// 完整图库模式下，输出目录里哪些文件出现/消失，完全由「这组实况会不会被转换」决定：
//   - 转换失败 → 原件原样送出（图库不能缺照片）
//   - 转换成功 → 原件副本必须删掉（否则下游 HEIC + JPG 重复）
func TestMirrorPlanFollowsJobStatus(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	out := filepath.Join(dir, "output")

	write := func(rel, body string) {
		p := filepath.Join(src, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a/IMG_0001.HEIC", "heic")
	write("a/IMG_0001.MOV", "mov")
	write("a/IMG_0002.HEIC", "plain")

	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()

	newAsset := func(rel, kind, cid string) *store.Asset {
		p := filepath.Join(src, filepath.FromSlash(rel))
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return &store.Asset{
			SourceRoot: src, RelPath: rel, Kind: kind,
			SizeBytes: fi.Size(), MtimeUnix: fi.ModTime().Unix(),
			State: store.AssetStateOK, LastScanID: 1,
		}
	}
	heic := newAsset("a/IMG_0001.HEIC", store.KindHEIC, "UUID-1")
	video := newAsset("a/IMG_0001.MOV", store.KindVideo, "UUID-1")
	plain := newAsset("a/IMG_0002.HEIC", store.KindHEIC, "")
	if _, err := st.UpsertAssets(ctx, []*store.Asset{heic, video, plain}); err != nil {
		t.Fatal(err)
	}
	if err := st.BulkSetContentID(ctx, []store.ContentIDUpdate{
		{ID: heic.ID, ContentID: "UUID-1", State: store.AssetStateOK},
		{ID: video.ID, ContentID: "UUID-1", State: store.AssetStateOK},
		{ID: plain.ID, State: store.AssetStateNoCID},
	}); err != nil {
		t.Fatal(err)
	}
	pairID, _, _, err := st.UpsertPair(ctx, &store.Pair{
		SourceRoot: src, ContentID: "UUID-1",
		HeicAssetID: heic.ID, VideoAssetID: video.ID,
		HeicRelPath: heic.RelPath, VideoRelPath: video.RelPath,
		HeicMtime: heic.MtimeUnix, VideoMtime: video.MtimeUnix,
		HeicSize: heic.SizeBytes, VideoSize: video.SizeBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	jobID, _, err := st.EnsureJob(ctx, pairID, 3)
	if err != nil {
		t.Fatal(err)
	}

	settings := config.DefaultSettings()
	settings.SourceDir = src
	settings.OutputDir = out
	settings.IncludeUnconverted = true
	settings.AutoScan = false
	// 这个用例验证的是「状态判定」，所以关掉观察期（0 = 不等待）。
	// 观察期本身由 TestMirrorHoldsUnsettledFiles 覆盖。
	settings.MirrorSettleMin = 0
	eng := engine.New(config.Config{DataDir: dir, LogLevel: "error", ExifBin: "missing", HeifBin: "missing"}, st, settings)

	// 1) 转换失败：三个文件都应被原样送出
	if err := st.FailJobPermanent(ctx, jobID, "模拟转换失败"); err != nil {
		t.Fatal(err)
	}
	stats, err := eng.MirrorNow(ctx)
	if err != nil {
		t.Fatalf("镜像失败: %v", err)
	}
	if stats.Copied != 3 {
		t.Fatalf("转换失败时三个文件都应复制，实际 %+v", stats)
	}
	for _, rel := range []string{"a/IMG_0001.HEIC", "a/IMG_0001.MOV", "a/IMG_0002.HEIC"} {
		if _, err := os.Stat(filepath.Join(out, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("%s 应存在于输出目录", rel)
		}
	}

	// 2) 转换成功：原件副本必须消失，普通照片保留
	if err := st.CompleteJob(ctx, jobID); err != nil {
		t.Fatal(err)
	}
	stats, err = eng.MirrorNow(ctx)
	if err != nil {
		t.Fatalf("镜像失败: %v", err)
	}
	if stats.Pruned != 2 {
		t.Fatalf("转换成功后应清理 2 个原件副本，实际 %+v", stats)
	}
	for _, rel := range []string{"a/IMG_0001.HEIC", "a/IMG_0001.MOV"} {
		if _, err := os.Stat(filepath.Join(out, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Fatalf("%s 已合成进 JPG，副本不应残留（err=%v）", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "a", "IMG_0002.HEIC")); err != nil {
		t.Fatalf("普通照片应保留: %v", err)
	}
}

// 观察期：刚落地的 HEIC/MOV、刚失败的配对都先不进输出目录。
//
// 这是「只写终态」的核心防线。真人场景：上游把实况的两半分批送来，
// HEIC 先到、MOV 几分钟后才到。若 HEIC 一到就复制出去，同步端会立刻把它当成
// 一张普通照片上传；等 MOV 到达、转换完成再删掉输出目录里的 HEIC，
// 上游那一份已经存在了——删除撤不回来，下游就是「静态照片 + 实况照片」两份。
func TestMirrorHoldsUnsettledFiles(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	out := filepath.Join(dir, "output")

	write := func(rel, body string) {
		p := filepath.Join(src, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a/IMG_0001.HEIC", "heic")
	write("a/IMG_0001.MOV", "mov")
	write("a/IMG_0002.HEIC", "plain")

	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	newAsset := func(rel, kind string) *store.Asset {
		p := filepath.Join(src, filepath.FromSlash(rel))
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return &store.Asset{
			SourceRoot: src, RelPath: rel, Kind: kind,
			SizeBytes: fi.Size(), MtimeUnix: fi.ModTime().Unix(),
			State: store.AssetStateOK, LastScanID: 1,
		}
	}
	heic := newAsset("a/IMG_0001.HEIC", store.KindHEIC)
	video := newAsset("a/IMG_0001.MOV", store.KindVideo)
	plain := newAsset("a/IMG_0002.HEIC", store.KindHEIC)
	if _, err := st.UpsertAssets(ctx, []*store.Asset{heic, video, plain}); err != nil {
		t.Fatal(err)
	}
	if err := st.BulkSetContentID(ctx, []store.ContentIDUpdate{
		{ID: heic.ID, ContentID: "UUID-1", State: store.AssetStateOK},
		{ID: video.ID, ContentID: "UUID-1", State: store.AssetStateOK},
		{ID: plain.ID, State: store.AssetStateNoCID},
	}); err != nil {
		t.Fatal(err)
	}
	pairID, _, _, err := st.UpsertPair(ctx, &store.Pair{
		SourceRoot: src, ContentID: "UUID-1",
		HeicAssetID: heic.ID, VideoAssetID: video.ID,
		HeicRelPath: heic.RelPath, VideoRelPath: video.RelPath,
		HeicMtime: heic.MtimeUnix, VideoMtime: video.MtimeUnix,
		HeicSize: heic.SizeBytes, VideoSize: video.SizeBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	jobID, _, err := st.EnsureJob(ctx, pairID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FailJobPermanent(ctx, jobID, "模拟转换失败"); err != nil {
		t.Fatal(err)
	}

	mk := func(settle int) *engine.Engine {
		s := config.DefaultSettings()
		s.SourceDir = src
		s.OutputDir = out
		s.IncludeUnconverted = true
		s.AutoScan = false
		s.MirrorSettleMin = settle
		return engine.New(config.Config{DataDir: dir, LogLevel: "error", ExifBin: "missing", HeifBin: "missing"}, st, s)
	}

	// 观察期内：刚失败的配对 + 刚发现的两个文件，一个都不许写出去
	stats, err := mk(30).MirrorNow(ctx)
	if err != nil {
		t.Fatalf("镜像失败: %v", err)
	}
	if stats.Copied != 0 {
		t.Fatalf("观察期内不应复制任何文件，实际 %+v", stats)
	}
	if stats.Held != 3 {
		t.Fatalf("三个文件都应处于观察中，实际 %+v", stats)
	}
	for _, rel := range []string{"a/IMG_0001.HEIC", "a/IMG_0001.MOV", "a/IMG_0002.HEIC"} {
		if _, err := os.Stat(filepath.Join(out, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Fatalf("%s 还在观察期，不应出现在输出目录", rel)
		}
	}

	// 观察期满（这里用 0 模拟）：三个文件都应当补上
	stats, err = mk(0).MirrorNow(ctx)
	if err != nil {
		t.Fatalf("镜像失败: %v", err)
	}
	if stats.Copied != 3 {
		t.Fatalf("观察期满后应补齐 3 个文件，实际 %+v", stats)
	}
	for _, rel := range []string{"a/IMG_0001.HEIC", "a/IMG_0001.MOV", "a/IMG_0002.HEIC"} {
		if _, err := os.Stat(filepath.Join(out, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("%s 观察期满后应被复制: %v", rel, err)
		}
	}
}
