//go:build integration

package convert_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/immich2motion/immich2motion/internal/config"
	"github.com/immich2motion/immich2motion/internal/convert"
	"github.com/immich2motion/immich2motion/internal/library"
	"github.com/immich2motion/immich2motion/internal/store"
)

// 运行方式（需要真实的 exiftool；heif 解码器可用脚本模拟）：
//
//	I2M_TEST_EXIF_BIN=/path/to/exiftool \
//	I2M_TEST_HEIF_BIN=/path/to/fake-heif-convert \
//	go test -tags integration ./internal/convert/ -v
func TestPipelineExecuteEndToEnd(t *testing.T) {
	heifBin := os.Getenv("I2M_TEST_HEIF_BIN")
	exifBin := os.Getenv("I2M_TEST_EXIF_BIN")
	if heifBin == "" || exifBin == "" {
		t.Skip("未设置 I2M_TEST_HEIF_BIN / I2M_TEST_EXIF_BIN，跳过集成测试")
	}

	dir := t.TempDir()
	srcDir := filepath.Join(dir, "source", "2024", "05")
	outDir := filepath.Join(dir, "output")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// 源 JPG（模拟 HEIC 解码前的载体，pipeline 只把它当输入文件交给 heif 解码器）
	heicPath := filepath.Join(srcDir, "IMG_0001.jpg")
	writeTestJPEG(t, heicPath)
	runExif(t, exifBin,
		"-overwrite_original", "-q", "-q",
		"-EXIF:DateTimeOriginal=2024:05:01 12:34:56",
		"-EXIF:CreateDate=2024:05:01 12:34:56",
		"-EXIF:Make=Apple",
		"-EXIF:Model=iPhone 15 Pro",
		"-EXIF:GPSLatitude=31.230416",
		"-EXIF:GPSLatitudeRef=N",
		"-EXIF:GPSLongitude=121.473701",
		"-EXIF:GPSLongitudeRef=E",
		heicPath,
	)

	// 源视频：真实字节，用于验证「原流拼接，不转码」
	videoBytes := bytes.Repeat([]byte{0x00, 0x00, 0x00, 0x18, 0x66, 0x74, 0x79, 0x70}, 512)
	videoPath := filepath.Join(srcDir, "IMG_0001.mov")
	if err := os.WriteFile(videoPath, videoBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	srcMD5Before, _, err := library.MD5File(heicPath)
	if err != nil {
		t.Fatal(err)
	}
	srcVideoMD5Before, _, err := library.MD5File(videoPath)
	if err != nil {
		t.Fatal(err)
	}

	settings := config.DefaultSettings()
	settings.SourceDir = filepath.Join(dir, "source")
	settings.OutputDir = outDir
	settings.JPGQuality = 92
	settings.OutputSuffix = "-motion"

	pipe := convert.NewPipeline(heifBin, exifBin)
	var stages []string
	res, err := pipe.Execute(context.Background(), convert.Job{
		JobID: 1,
		Pair: store.Pair{
			ID:           1,
			SourceRoot:   settings.SourceDir,
			ContentID:    "UUID-TEST",
			HeicRelPath:  "2024/05/IMG_0001.jpg",
			VideoRelPath: "2024/05/IMG_0001.mov",
		},
		Settings: settings,
		Progress: func(stage string, pct float64) {
			if len(stages) == 0 || stages[len(stages)-1] != stage {
				stages = append(stages, stage)
			}
		},
	})
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}

	// 1) 输出路径保留目录结构并带后缀
	if res.RelPath != filepath.ToSlash("2024/05/IMG_0001-motion.jpg") {
		t.Fatalf("输出相对路径错误: %s", res.RelPath)
	}

	// 2) 文件大小与分段字节数一致
	fi, err := os.Stat(res.AbsPath)
	if err != nil {
		t.Fatalf("输出文件不存在: %v", err)
	}
	if fi.Size() != res.JPGBytes+res.VideoBytes {
		t.Fatalf("大小不一致: 文件 %d, 期望 %d", fi.Size(), res.JPGBytes+res.VideoBytes)
	}
	if res.VideoBytes != int64(len(videoBytes)) {
		t.Fatalf("视频字节数应与源文件完全一致（不转码）: %d vs %d", res.VideoBytes, len(videoBytes))
	}

	// 3) JPG 段是合法 JPEG（FFD8 开头、FFD9 结尾）
	raw, err := os.ReadFile(res.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, []byte{0xFF, 0xD8}) {
		t.Fatal("输出不以 JPEG SOI 开头")
	}
	jpgPart := raw[:res.JPGBytes]
	if !bytes.HasSuffix(jpgPart, []byte{0xFF, 0xD9}) {
		t.Fatal("JPG 段不以 EOI 结尾")
	}
	if !bytes.Equal(raw[res.JPGBytes:], videoBytes) {
		t.Fatal("offset 之后必须是原始视频字节，逐字节相等")
	}

	// 4) XMP：MicroVideo=1 且 MicroVideoOffset == JPG 段长度
	meta := readExifJSON(t, exifBin, res.AbsPath,
		"-MicroVideo", "-MicroVideoOffset", "-MicroVideoVersion",
		"-EXIF:DateTimeOriginal", "-EXIF:Model", "-EXIF:GPSLatitude", "-EXIF:GPSLongitude")

	if got := pickTag(meta, "MicroVideo"); got != "1" {
		t.Fatalf("MicroVideo 应为 1，实际 %q（全部标签: %v）", got, meta)
	}
	offsetStr := pickTag(meta, "MicroVideoOffset")
	offset, err := strconv.ParseInt(offsetStr, 10, 64)
	if err != nil {
		// exiftool 13.x 会把大整数输出成科学计数法
		if f, ferr := strconv.ParseFloat(offsetStr, 64); ferr == nil {
			offset, err = int64(f), nil
		}
	}
	if err != nil {
		t.Fatalf("MicroVideoOffset 非法: %q", offsetStr)
	}
	if offset != res.JPGBytes {
		t.Fatalf("MicroVideoOffset=%d 必须等于 JPG 字节数 %d", offset, res.JPGBytes)
	}

	// 5) EXIF 时间与 GPS 已保留
	if got := pickTag(meta, "DateTimeOriginal"); got == "" {
		t.Error("DateTimeOriginal 未复制到输出")
	}
	if got := pickTag(meta, "Model"); !strings.Contains(got, "iPhone 15 Pro") {
		t.Errorf("Model 未复制: %q", got)
	}
	if pickTag(meta, "GPSLatitude") == "" || pickTag(meta, "GPSLongitude") == "" {
		t.Errorf("GPS 未复制: %v", meta)
	}

	// 6) 输出 MD5 与返回值一致，可直接用于后续校验
	actualMD5, _, err := library.MD5File(res.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	if actualMD5 != res.MD5 {
		t.Fatalf("MD5 不匹配: %s vs %s", actualMD5, res.MD5)
	}

	// 7) 源文件绝对不能被修改
	srcMD5After, _, _ := library.MD5File(heicPath)
	videoMD5After, _, _ := library.MD5File(videoPath)
	if srcMD5Before != srcMD5After || srcVideoMD5Before != videoMD5After {
		t.Fatal("源文件被修改了！源目录必须只读")
	}

	// 8) 临时文件必须清理干净
	entries, _ := os.ReadDir(filepath.Dir(res.AbsPath))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".i2m-") {
			t.Errorf("残留临时文件: %s", e.Name())
		}
	}

	// 9) 阶段进度应按顺序推进
	if len(stages) < 4 {
		t.Fatalf("进度阶段过少: %v", stages)
	}
	t.Logf("阶段顺序: %v", stages)
}

// 输出路径不允许越出输出目录，也不允许写进源目录。
func TestOutputPathGuards(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	out := filepath.Join(dir, "output")
	os.MkdirAll(src, 0o755)
	os.MkdirAll(out, 0o755)

	settings := config.DefaultSettings()
	settings.SourceDir = src
	settings.OutputDir = out
	settings.OutputSuffix = "-motion"

	if got := convert.OutputRelPath(settings.OutputSuffix, "a/b/IMG.HEIC"); got != "a/b/IMG-motion.jpg" {
		t.Fatalf("路径生成错误: %s", got)
	}
	if got := convert.OutputRelPath(settings.OutputSuffix, "IMG.HEIC"); got != "IMG-motion.jpg" {
		t.Fatalf("根目录路径生成错误: %s", got)
	}
	// 输出目录在源目录内部应被配置校验拒绝
	settings.OutputDir = filepath.Join(src, "motion")
	if err := settings.Validate(); err == nil {
		t.Fatal("输出目录位于源目录内部时应校验失败")
	}
}

func writeTestJPEG(t *testing.T, path string) {
	t.Helper()
	const w, h = 120, 90
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 2), G: uint8(y * 2), B: 160, A: 255})
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

func runExif(t *testing.T, bin string, args ...string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("exiftool %v 失败: %v\n%s", args, err, out)
	}
}

// readExifJSON 读取指定标签，返回不带组名前缀的键值对。
func readExifJSON(t *testing.T, bin, path string, tags ...string) map[string]string {
	t.Helper()
	args := append([]string{"-j", "-G0", "-s", "-a", "-q", "-q"}, tags...)
	args = append(args, path)
	out, err := exec.Command(bin, args...).Output()
	if err != nil {
		t.Fatalf("读取元数据失败: %v", err)
	}
	var entries []map[string]any
	if err := json.Unmarshal(out, &entries); err != nil || len(entries) == 0 {
		t.Fatalf("解析元数据失败: %v (%s)", err, out)
	}
	res := make(map[string]string, len(entries[0]))
	for k, v := range entries[0] {
		if k == "SourceFile" {
			continue
		}
		res[k] = strings.TrimSpace(strings.Trim(strings.TrimSpace(toStr(v)), `"`))
	}
	return res
}

func toStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// pickTag 按标签名后缀查找：exiftool 的组名会随文件类型变化
// （同一个标签可能是 XMP:MicroVideo 或 XMP-GCamera:MicroVideo），
// 因此只能按名字匹配，这也正是生产代码的校验方式。
func pickTag(m map[string]string, name string) string {
	for k, v := range m {
		if strings.HasSuffix(k, ":"+name) || k == name {
			return v
		}
	}
	return ""
}
