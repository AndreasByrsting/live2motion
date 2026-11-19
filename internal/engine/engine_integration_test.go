//go:build integration

package engine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndreasByrsting/live2motion/internal/config"
	"github.com/AndreasByrsting/live2motion/internal/engine"
	"github.com/AndreasByrsting/live2motion/internal/library"
	"github.com/AndreasByrsting/live2motion/internal/store"
)

// 端到端验证：扫描 → 读 ContentIdentifier → MD5 → 配对 → 入队 → worker 转换 → 落盘 → 数据库状态。
//
// 真实 HEIC 无法在测试环境构造，因此这里用「真实 JPEG + 真实 MP4」替代 HEIC 载体：
// heic_exts 配成 .jpg，ContentIdentifier 直接写入数据库（模拟 exiftool 从 Apple MakerNotes
// 读到 UUID 的结果）。除 HEIC 解码本身，其余链路全部真实执行。
func TestEngineScanPairConvert(t *testing.T) {
	exifBin := os.Getenv("L2M_TEST_EXIF_BIN")
	heifBin := os.Getenv("L2M_TEST_HEIF_BIN")
	if exifBin == "" || heifBin == "" {
		t.Skip("未设置 L2M_TEST_EXIF_BIN / L2M_TEST_HEIF_BIN，跳过集成测试")
	}

	root := t.TempDir()
	srcDir := filepath.Join(root, "source", "2024", "05")
	outDir := filepath.Join(root, "output")
	dataDir := filepath.Join(root, "data")
	for _, d := range []string{srcDir, outDir, dataDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// 素材 1：可配对的 Live Photo
	photo := filepath.Join(srcDir, "IMG_0001.jpg")
	writeJPEG(t, photo)
	runExif(t, exifBin, "-overwrite_original", "-q", "-q",
		"-EXIF:DateTimeOriginal=2024:05:01 12:34:56",
		"-EXIF:Model=iPhone 15 Pro",
		"-EXIF:GPSLatitude=31.230416", "-EXIF:GPSLatitudeRef=N",
		"-EXIF:GPSLongitude=121.473701", "-EXIF:GPSLongitudeRef=E",
		photo)

	mov := filepath.Join(srcDir, "IMG_0001.mov")
	writeMP4(t, mov)
	const cidA = "5F3A9C1E-LIVE-UUID-0001"
	runExif(t, exifBin, "-q", "-q", "-Keys:ContentIdentifier="+cidA, mov)

	// 素材 2：普通照片（无 ContentIdentifier），不应产生任务
	plain := filepath.Join(srcDir, "IMG_0002.jpg")
	writeJPEG(t, plain)

	st, err := store.Open(filepath.Join(dataDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	settings := config.DefaultSettings()
	settings.SourceDir = filepath.Join(root, "source")
	settings.OutputDir = outDir
	settings.HeicExts = []string{".jpg"}
	settings.VideoExts = []string{".mov"}
	settings.Workers = 1
	settings.AutoScan = false
	settings.ScanIntervalSec = 3600
	settings.JPGQuality = 92

	cfg := config.Config{DataDir: dataDir, ExifBin: exifBin, HeifBin: heifBin, LogLevel: "warn"}
	eng := engine.New(cfg, st, settings)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// --- 第一轮扫描：登记文件、计算 MD5，HEIC 侧因无 CID 标记为 no_cid ---
	first, err := eng.ScanNow(ctx, "test-1")
	if err != nil {
		t.Fatalf("首次扫描失败: %v", err)
	}
	if first.FilesSeen != 3 {
		t.Fatalf("应扫描到 3 个文件（2 个照片 + 1 个视频），实际 %d", first.FilesSeen)
	}
	if first.HeicSeen != 2 || first.VideoSeen != 1 {
		t.Fatalf("分类统计错误: heic=%d video=%d", first.HeicSeen, first.VideoSeen)
	}
	if first.PairsTotal != 0 {
		t.Fatalf("首轮不应有配对，实际 %d", first.PairsTotal)
	}

	// 视频侧应已通过真实 exiftool 读到 UUID
	var videoCID string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COALESCE(content_id,'') FROM live_assets WHERE rel_path='2024/05/IMG_0001.mov'`).Scan(&videoCID); err != nil {
		t.Fatal(err)
	}
	if videoCID != cidA {
		t.Fatalf("视频 ContentIdentifier 读取失败: %q", videoCID)
	}

	// --- 模拟 exiftool 从 HEIC MakerNotes 读到同一个 UUID ---
	var photoID int64
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM live_assets WHERE rel_path='2024/05/IMG_0001.jpg'`).Scan(&photoID); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAssetContentID(ctx, photoID, cidA, "test"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAssetState(ctx, photoID, store.AssetStateOK, ""); err != nil {
		t.Fatal(err)
	}

	// --- 第二轮扫描：UUID 一致 → 配对 → 入队 ---
	second, err := eng.ScanNow(ctx, "test-2")
	if err != nil {
		t.Fatalf("二次扫描失败: %v", err)
	}
	if second.PairsTotal != 1 || second.PairsNew != 1 {
		t.Fatalf("应新增 1 组配对，实际 total=%d new=%d", second.PairsTotal, second.PairsNew)
	}
	// 普通照片（无 CID）不应被算作孤儿
	if second.PairsUnpaired != 0 {
		t.Fatalf("普通照片不应计入未配对: %d", second.PairsUnpaired)
	}

	// --- 启动引擎，等待 worker 完成转换 ---
	if err := eng.Start(ctx, false); err != nil {
		t.Fatal(err)
	}
	defer eng.Stop()

	deadline := time.Now().Add(60 * time.Second)
	var stats store.JobStats
	for time.Now().Before(deadline) {
		stats, err = st.JobStats(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if stats.Success+stats.Failed > 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if stats.Failed > 0 {
		jobs, _, _ := st.ListJobs(ctx, store.JobFilter{Status: store.JobFailed})
		for _, j := range jobs {
			t.Logf("失败任务 %d: %s", j.JobID, j.LastError)
		}
		t.Fatalf("存在失败任务: %d", stats.Failed)
	}
	if stats.Success != 1 {
		t.Fatalf("期望 1 个成功任务，实际 %+v", stats)
	}

	// --- 校验输出 ---
	outPath := filepath.Join(outDir, "2024", "05", "IMG_0001-motion.jpg")
	fi, err := os.Stat(outPath)
	if err != nil {
		t.Fatalf("输出文件不存在: %v", err)
	}

	jobs, _, err := st.ListJobs(ctx, store.JobFilter{Status: store.JobSuccess})
	if err != nil || len(jobs) != 1 {
		t.Fatalf("读取成功任务失败: %v (%d)", err, len(jobs))
	}
	out, err := st.GetOutputByPair(ctx, jobs[0].PairID)
	if err != nil || out == nil {
		t.Fatalf("产物记录缺失: %v", err)
	}
	if out.MD5 == "" {
		t.Fatal("产物 MD5 未记录")
	}
	if fi.Size() != out.SizeBytes {
		t.Fatalf("产物大小不一致: 文件 %d / 记录 %d", fi.Size(), out.SizeBytes)
	}
	if out.JPGBytes+out.VideoBytes != out.SizeBytes {
		t.Fatalf("分段字节数不自洽: %d + %d != %d", out.JPGBytes, out.VideoBytes, out.SizeBytes)
	}

	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, []byte{0xFF, 0xD8}) {
		t.Fatal("输出不是 JPEG 容器")
	}
	movRaw, _ := os.ReadFile(mov)
	if !bytes.HasSuffix(raw, movRaw) {
		t.Fatal("输出尾部不是原始视频字节")
	}

	// XMP 与 EXIF 落到产物上
	meta := readTags(t, exifBin, outPath)
	if meta["MicroVideo"] != "1" {
		t.Fatalf("MicroVideo 未写入: %v", meta)
	}
	if meta["MicroVideoOffset"] != fmt.Sprint(out.JPGBytes) {
		t.Fatalf("MicroVideoOffset=%s 与 JPG 段 %d 不符", meta["MicroVideoOffset"], out.JPGBytes)
	}
	if meta["DateTimeOriginal"] == "" {
		t.Error("EXIF 拍照时间丢失")
	}
	if meta["GPSLatitude"] == "" {
		t.Error("EXIF GPS 丢失")
	}

	// 源文件必须保持原样
	if md5, _, err := library.MD5File(photo); err != nil || md5 == "" {
		t.Fatalf("源文件读取异常: %v", err)
	}

	// 任务状态与产物计数
	stats2, _ := st.JobStats(ctx)
	if stats2.Success != 1 || stats2.Pending != 0 || stats2.Running != 0 {
		t.Fatalf("任务状态异常: %+v", stats2)
	}
	if n, _ := st.CountOutputs(ctx, settings.SourceDir); n != 1 {
		t.Fatalf("产物计数错误: %d", n)
	}

	// 重复扫描不应重复入队（增量幂等）
	third, err := eng.ScanNow(ctx, "test-3")
	if err != nil {
		t.Fatal(err)
	}
	if third.PairsNew != 0 {
		t.Fatalf("重复扫描不应新增配对: %d", third.PairsNew)
	}
	stats3, _ := st.JobStats(ctx)
	if stats3.Pending != 0 {
		t.Fatalf("重复扫描不应产生新任务: %+v", stats3)
	}
}

func writeJPEG(t *testing.T, path string) {
	t.Helper()
	const w, h = 100, 80
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 140, A: 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
}

// writeMP4 生成最小合法 MP4（ftyp + moov/mvhd），使 exiftool 可写入 QuickTime Keys。
func writeMP4(t *testing.T, path string) {
	t.Helper()
	box := func(typ string, payload []byte) []byte {
		b := make([]byte, 8+len(payload))
		b[0] = byte((8 + len(payload)) >> 24)
		b[1] = byte((8 + len(payload)) >> 16)
		b[2] = byte((8 + len(payload)) >> 8)
		b[3] = byte(8 + len(payload))
		copy(b[4:8], typ)
		copy(b[8:], payload)
		return b
	}
	mvhd := make([]byte, 100)
	mvhd[15] = 0xE8 // timescale = 1000
	mvhd[23] = 1    // rate
	mvhd[24] = 1    // volume
	mvhd[39] = 1    // matrix
	mvhd[51] = 1
	mvhd[59] = 0x40
	mvhd[99] = 1
	data := append(box("ftyp", []byte("mp42\x00\x00\x00\x00mp42isom")), box("moov", box("mvhd", mvhd))...)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func runExif(t *testing.T, bin string, args ...string) {
	t.Helper()
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("exiftool %v 失败: %v\n%s", args, err, out)
	}
}

func readTags(t *testing.T, bin, path string) map[string]string {
	t.Helper()
	out, err := exec.Command(bin, "-j", "-G0", "-s", "-a", "-q", "-q",
		"-MicroVideo", "-MicroVideoOffset",
		"-EXIF:DateTimeOriginal", "-EXIF:GPSLatitude", path).Output()
	if err != nil {
		t.Fatalf("读取元数据失败: %v", err)
	}
	res := map[string]string{}
	var entries []map[string]any
	if err := json.Unmarshal(out, &entries); err != nil || len(entries) == 0 {
		t.Fatalf("解析元数据失败: %v", err)
	}
	for k, v := range entries[0] {
		name := k
		if i := strings.LastIndex(k, ":"); i >= 0 {
			name = k[i+1:]
		}
		if s, ok := v.(string); ok {
			res[name] = s
		} else {
			res[name] = fmt.Sprint(v)
		}
	}
	return res
}
