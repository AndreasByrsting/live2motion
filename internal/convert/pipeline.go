package convert

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/AndreasByrsting/live2motion/internal/config"
	"github.com/AndreasByrsting/live2motion/internal/library"
	"github.com/AndreasByrsting/live2motion/internal/store"
)

// Pipeline 执行单组 Live Photo 的转换：HEIC→JPG、写 XMP、拼接 MOV、校验、原子落盘。
type Pipeline struct {
	HeifBin string
	ExifBin string
}

// NewPipeline 构造转换流水线。
func NewPipeline(heifBin, exifBin string) *Pipeline {
	return &Pipeline{HeifBin: heifBin, ExifBin: exifBin}
}

// Job 一次转换的输入。
type Job struct {
	JobID    int64
	Pair     store.Pair
	Settings config.Settings
	Progress func(stage string, pct float64)
}

// Result 一次转换的产物信息。
type Result struct {
	RelPath    string
	AbsPath    string
	SizeBytes  int64
	JPGBytes   int64
	VideoBytes int64
	MD5        string
}

// 各阶段进度百分比，前端进度条据此显示。
const (
	stagePrepare = "prepare"
	stageHEIC    = "heic->jpg"
	stageXMP     = "xmp"
	stageConcat  = "concat"
	stageVerify  = "verify"
	stageDone    = "done"
)

// Execute 执行一次完整转换。源文件只以只读方式打开，输出先写同目录临时文件，
// 校验通过后再原子 rename，避免下游同步到半成品。
func (p *Pipeline) Execute(ctx context.Context, job Job) (*Result, error) {
	st := job.Settings
	report := job.Progress
	if report == nil {
		report = func(string, float64) {}
	}

	if err := st.Validate(); err != nil {
		return nil, err
	}

	heicAbs := filepath.Join(st.SourceDir, filepath.FromSlash(job.Pair.HeicRelPath))
	videoAbs := filepath.Join(st.SourceDir, filepath.FromSlash(job.Pair.VideoRelPath))
	for _, f := range []string{heicAbs, videoAbs} {
		if _, err := os.Stat(f); err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("源文件已不存在: %s", f)
			}
			return nil, fmt.Errorf("源文件不可访问 %s: %w", f, err)
		}
	}

	relOut := OutputRelPath(st.OutputSuffix, job.Pair.HeicRelPath)
	absOut := filepath.Join(st.OutputDir, filepath.FromSlash(relOut))
	// 双重保险：输出绝不允许落在源目录内，也绝不允许越出输出根目录。
	if err := ensureInside(absOut, st.OutputDir); err != nil {
		return nil, fmt.Errorf("输出路径越界: %w", err)
	}
	if err := ensureOutside(absOut, st.SourceDir); err != nil {
		return nil, err
	}
	outDir := filepath.Dir(absOut)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建输出目录失败: %w", err)
	}

	report(stagePrepare, 0.02)

	// 1) HEIC → JPG（临时文件与最终产物同目录，保证 rename 不跨设备）
	tmpJPG, err := os.CreateTemp(outDir, ".l2m-*.jpg")
	if err != nil {
		return nil, fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpJPGPath := tmpJPG.Name()
	tmpJPG.Close()
	os.Remove(tmpJPGPath) // 让 heif-convert 自己创建，避免权限残留
	defer os.Remove(tmpJPGPath)

	report(stageHEIC, 0.05)
	if err := p.heicToJPG(ctx, heicAbs, tmpJPGPath, st.JPGQuality); err != nil {
		return nil, fmt.Errorf("HEIC 转 JPG 失败: %w", err)
	}
	report(stageHEIC, 0.40)

	// 2) 复制 EXIF 时间/GPS 并写入 MicroVideo / MicroVideoOffset
	//    exiftool 写 XMP 会改变文件长度，而 MicroVideoOffset 必须等于最终 JPG 长度，
	//    所以这里迭代收敛：写入 offset → 重新量长度 → 直到两者相等。
	report(stageXMP, 0.45)
	if err := p.embedMotionXMP(ctx, heicAbs, tmpJPGPath, st.PreserveOrientation); err != nil {
		return nil, err
	}
	jpgLen, err := fileSize(tmpJPGPath)
	if err != nil {
		return nil, fmt.Errorf("读取 JPG 大小失败: %w", err)
	}
	if err := p.verifyXMP(ctx, tmpJPGPath, jpgLen); err != nil {
		return nil, err
	}
	report(stageXMP, 0.62)

	// 3) 拼接：JPG 直接流式接 MOV，不转码不动源文件
	tmpOut, err := os.CreateTemp(outDir, ".l2m-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("创建输出临时文件失败: %w", err)
	}
	tmpOutPath := tmpOut.Name()
	defer os.Remove(tmpOutPath)

	videoLen, copyErr := concatFiles(tmpOut, tmpJPGPath, videoAbs)
	closeErr := tmpOut.Close()
	if copyErr != nil {
		return nil, fmt.Errorf("拼接视频失败: %w", copyErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("写入输出文件失败: %w", closeErr)
	}
	report(stageConcat, 0.85)

	// concat 返回的是写入总字节数，减去 JPG 段才是视频段
	videoBytes := videoLen - jpgLen
	if videoBytes < 0 {
		return nil, fmt.Errorf("拼接结果异常：总长 %d 小于 JPG 段 %d", videoLen, jpgLen)
	}
	// 视频必须原流拼接，字节数与源文件完全一致（不转码、不截断）
	if vfi, statErr := os.Stat(videoAbs); statErr == nil && vfi.Size() != videoBytes {
		return nil, fmt.Errorf("视频字节数不一致：源 %d，写入 %d", vfi.Size(), videoBytes)
	}

	total := jpgLen + videoBytes
	if fi, err := os.Stat(tmpOutPath); err != nil {
		return nil, fmt.Errorf("输出文件不可读: %w", err)
	} else if fi.Size() != total {
		return nil, fmt.Errorf("输出文件大小异常: 期望 %d 实际 %d", total, fi.Size())
	}

	// 4) 计算输出 MD5，供后续完整性校验
	report(stageVerify, 0.90)
	sum, _, err := library.MD5File(tmpOutPath)
	if err != nil {
		return nil, fmt.Errorf("计算输出 MD5 失败: %w", err)
	}

	// 5) 原子落盘
	if err := os.Chmod(tmpOutPath, 0o644); err != nil {
		return nil, fmt.Errorf("设置输出权限失败: %w", err)
	}
	if err := os.Rename(tmpOutPath, absOut); err != nil {
		return nil, fmt.Errorf("输出落盘失败: %w", err)
	}
	report(stageDone, 1.0)

	return &Result{
		RelPath:    relOut,
		AbsPath:    absOut,
		SizeBytes:  total,
		JPGBytes:   jpgLen,
		VideoBytes: videoBytes,
		MD5:        sum,
	}, nil
}

// OutputRelPath 按「保留原目录结构 + 可配置后缀」计算输出相对路径。
func OutputRelPath(suffix, heicRel string) string {
	dir := path.Dir(heicRel)
	base := path.Base(heicRel)
	stem := strings.TrimSuffix(base, path.Ext(base))
	name := stem + suffix + ".jpg"
	if dir == "." || dir == "/" || dir == "" {
		return name
	}
	return path.Join(dir, name)
}

// heicToJPG 调用 heif-convert，质量参数 0-100。
func (p *Pipeline) heicToJPG(ctx context.Context, src, dst string, quality int) error {
	if quality < 1 || quality > 100 {
		quality = 90
	}
	args := []string{"-q", strconv.Itoa(quality), src, dst}
	if _, err := run(ctx, 10*time.Minute, p.HeifBin, args...); err != nil {
		return err
	}
	if fi, err := os.Stat(dst); err != nil || fi.Size() == 0 {
		return fmt.Errorf("heif-convert 未产出有效 JPG: %s", dst)
	}
	return nil
}

// exifTagsToCopy 需要保留的 EXIF 字段（拍照时间、GPS、机型、曝光参数）。
// 刻意复制 Orientation：heif-convert 已把 irot 变换应用到像素，
// 再带上原始 Orientation 会导致看图软件二次旋转（可用 PreserveOrientation 开关调整）。
var exifTagsToCopy = []string{
	"-EXIF:DateTimeOriginal",
	"-EXIF:CreateDate",
	"-EXIF:ModifyDate",
	"-EXIF:OffsetTime",
	"-EXIF:OffsetTimeOriginal",
	"-EXIF:OffsetTimeDigitized",
	"-EXIF:SubSecTimeOriginal",
	"-EXIF:SubSecTimeDigitized",
	"-EXIF:GPS*",
	"-EXIF:Make",
	"-EXIF:Model",
	"-EXIF:Software",
	"-EXIF:LensMake",
	"-EXIF:LensModel",
	"-EXIF:LensInfo",
	"-EXIF:FNumber",
	"-EXIF:ExposureTime",
	"-EXIF:ExposureProgram",
	"-EXIF:ISO",
	"-EXIF:FocalLength",
	"-EXIF:FocalLengthIn35mmFormat",
	"-EXIF:Flash",
	"-EXIF:MeteringMode",
	"-EXIF:WhiteBalance",
	"-EXIF:Artist",
	"-EXIF:Copyright",
}

// maxOffsetIterations 迭代上限，正常 2-3 次即可收敛。
const maxOffsetIterations = 6

// embedMotionXMP 迭代写入 MicroVideo 与 MicroVideoOffset，直到 offset 等于文件实际长度。
func (p *Pipeline) embedMotionXMP(ctx context.Context, heicPath, jpgPath string, keepOrientation bool) error {
	offset := int64(0)
	for i := 0; i < maxOffsetIterations; i++ {
		if err := p.writeXMPOnce(ctx, heicPath, jpgPath, offset, i == 0, keepOrientation); err != nil {
			return fmt.Errorf("写入 XMP 失败: %w", err)
		}
		size, err := fileSize(jpgPath)
		if err != nil {
			return fmt.Errorf("读取 JPG 大小失败: %w", err)
		}
		if size == offset {
			return nil
		}
		offset = size
	}
	return fmt.Errorf("MicroVideoOffset 迭代 %d 次仍未收敛，请确认 exiftool 版本", maxOffsetIterations)
}

// writeXMPOnce 一次 exiftool 调用：首次顺带复制 EXIF，之后只改 offset。
func (p *Pipeline) writeXMPOnce(ctx context.Context, heicPath, jpgPath string, offset int64, copyExif, keepOrientation bool) error {
	args := []string{"-overwrite_original", "-q", "-q", "-m"}
	if copyExif {
		args = append(args, "-TagsFromFile", heicPath)
		args = append(args, exifTagsToCopy...)
		if keepOrientation {
			args = append(args, "-EXIF:Orientation")
		}
	}
	args = append(args,
		"-XMP-GCamera:MicroVideo=1",
		"-XMP-GCamera:MicroVideoVersion=1",
		"-XMP-GCamera:MicroVideoOffset="+strconv.FormatInt(offset, 10),
		jpgPath,
	)
	_, err := run(ctx, 2*time.Minute, p.ExifBin, args...)
	return err
}

// verifyXMP 回读校验：MicroVideo 必须为 1，MicroVideoOffset 必须等于 JPG 字节数。
func (p *Pipeline) verifyXMP(ctx context.Context, jpgPath string, expectOffset int64) error {
	out, err := run(ctx, time.Minute, p.ExifBin,
		"-j", "-G0", "-s", "-MicroVideo", "-MicroVideoOffset", jpgPath)
	if err != nil {
		return fmt.Errorf("校验 XMP 失败: %w", err)
	}
	var entries []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &entries); err != nil || len(entries) == 0 {
		return fmt.Errorf("XMP 校验输出无法解析")
	}
	var mv string
	var offsetStr string
	for k, v := range entries[0] {
		switch {
		case strings.HasSuffix(k, "MicroVideo"):
			mv = fmt.Sprint(v)
		case strings.HasSuffix(k, "MicroVideoOffset"):
			offsetStr = fmt.Sprint(v)
		}
	}
	if mv != "1" {
		return fmt.Errorf("XMP MicroVideo 写入异常: %q", mv)
	}
	got, perr := parseIntLoose(offsetStr)
	if perr != nil {
		return fmt.Errorf("XMP MicroVideoOffset 缺失或非法: %q", offsetStr)
	}
	if got != expectOffset {
		return fmt.Errorf("XMP MicroVideoOffset=%d 与 JPG 实际长度 %d 不一致", got, expectOffset)
	}
	return nil
}

// parseIntLoose 兼容 exiftool 不同版本的 JSON 数值输出。
// 旧版把大整数输出成字符串 "3014691"，新版（13.x）会输出科学计数法 "3.014691e+06"，
// 真实照片动辄 2-4MB，offset 必然超过浮点友好阈值，因此必须两种都认。
func parseIntLoose(s string) (int64, error) {
	s = strings.TrimSpace(strings.Trim(s, `"`))
	if s == "" {
		return 0, fmt.Errorf("空值")
	}
	if v, err := strconv.ParseInt(s, 10, 64); err == nil {
		return v, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	return int64(math.Round(f)), nil
}

// concatFiles 把 parts 依次写入 dst，返回写入的总字节数。
func concatFiles(dst io.Writer, parts ...string) (int64, error) {
	buf := make([]byte, 1<<20)
	var total int64
	for _, p := range parts {
		f, err := os.Open(p) // 只读
		if err != nil {
			return total, err
		}
		n, err := io.CopyBuffer(dst, f, buf)
		total += n
		cerr := f.Close()
		if err != nil {
			return total, err
		}
		if cerr != nil {
			return total, cerr
		}
	}
	return total, nil
}

func fileSize(p string) (int64, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// ensureInside 校验 target 位于 root 之内。
func ensureInside(target, root string) error {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s 不在 %s 内", target, root)
	}
	return nil
}

// ensureOutside 校验 target 不在 sourceRoot 内（源目录强制只读）。
func ensureOutside(target, sourceRoot string) error {
	rel, err := filepath.Rel(sourceRoot, target)
	if err != nil {
		return nil // 不同盘符，必然不在源目录内
	}
	if rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)) {
		return fmt.Errorf("拒绝写入源目录: %s", target)
	}
	return nil
}
