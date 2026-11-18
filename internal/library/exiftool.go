package library

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// CIDResult 单个文件的 ContentIdentifier 读取结果。
type CIDResult struct {
	ContentID string
	Source    string // 命中的 exiftool 标签组，便于排查
	Err       string
}

// cidTagPriority 不同容器里 ContentIdentifier 所在的标签组不同，
// 按可信度从高到低选择，避免同一文件返回多个值时取错。
var cidTagPriority = []string{
	"XMP:ContentIdentifier",
	"EXIF:ContentIdentifier",
	"MakerNotes:ContentIdentifier",
	"Keys:ContentIdentifier",
	"QuickTime:ContentIdentifier",
	"UserData:ContentIdentifier",
	"ItemList:ContentIdentifier",
	"ContentIdentifier",
}

const exifBatchSize = 200

// ReadContentIdentifiers 用 exiftool 批量读取 ContentIdentifier。
// 返回 map[绝对路径]CIDResult；单个文件失败不会中断整批。
func ReadContentIdentifiers(ctx context.Context, bin string, paths []string) (map[string]CIDResult, error) {
	out := make(map[string]CIDResult, len(paths))
	for start := 0; start < len(paths); start += exifBatchSize {
		end := start + exifBatchSize
		if end > len(paths) {
			end = len(paths)
		}
		batch := paths[start:end]
		if err := readCIDBatch(ctx, bin, batch, out); err != nil {
			// 整批失败（多半是二进制缺失）时向上抛，由调用方决定是否中止扫描。
			return out, err
		}
	}
	return out, nil
}

func readCIDBatch(ctx context.Context, bin string, batch []string, out map[string]CIDResult) error {
	args := make([]string, 0, len(batch)+6)
	args = append(args, "-j", "-G0", "-s", "-a", "-q", "-q", "-ContentIdentifier")
	args = append(args, batch...)

	cctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if cctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("exiftool 读取超时（批次 %d 个文件）", len(batch))
	}
	// exiftool 对部分文件报错时退出码非 0，但 stdout 仍然是合法 JSON，优先解析。
	res, parseErr := parseExifJSON(stdout.Bytes())
	if parseErr != nil {
		if err != nil {
			return fmt.Errorf("exiftool 执行失败: %w (%s)", err, strings.TrimSpace(stderr.String()))
		}
		return fmt.Errorf("解析 exiftool 输出失败: %w", parseErr)
	}

	for i, path := range batch {
		if i < len(res) {
			out[path] = pickCID(res[i])
			continue
		}
		out[path] = CIDResult{Err: "exiftool 未返回该文件记录"}
	}
	return nil
}

func parseExifJSON(raw []byte) ([]map[string]any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("空输出")
	}
	if trimmed[0] != '[' {
		// 单个文件时 exiftool 仍返回数组；非数组视为异常输出。
		return nil, fmt.Errorf("非 JSON 数组输出")
	}
	var out []map[string]any
	if err := json.Unmarshal(trimmed, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func pickCID(entry map[string]any) CIDResult {
	if entry == nil {
		return CIDResult{Err: "无元数据"}
	}
	values := make(map[string]string, 4)
	for k, v := range entry {
		if !strings.EqualFold(lastSegment(k), "ContentIdentifier") {
			continue
		}
		s, ok := v.(string)
		if !ok {
			s = fmt.Sprint(v)
		}
		if cid := normalizeCID(s); cid != "" {
			values[k] = cid
		}
	}
	for _, key := range cidTagPriority {
		if v, ok := values[key]; ok && v != "" {
			return CIDResult{ContentID: v, Source: key}
		}
	}
	// 兜底：任意组的 ContentIdentifier
	for k, v := range values {
		if v != "" {
			return CIDResult{ContentID: v, Source: k}
		}
	}
	if msg, ok := entry["Error"].(string); ok && msg != "" {
		return CIDResult{Err: msg}
	}
	return CIDResult{}
}

func lastSegment(tag string) string {
	if i := strings.LastIndex(tag, ":"); i >= 0 {
		return tag[i+1:]
	}
	return tag
}

// normalizeCID 规范化 UUID：去空白、去引号、统一大写。
// 配对比较只依赖这个规范化后的值，绝不使用文件名。
func normalizeCID(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "\"'")
	s = strings.ReplaceAll(s, "\u200b", "")
	s = strings.ReplaceAll(s, "\u00a0", "")
	s = strings.ToUpper(strings.TrimSpace(s))
	return s
}

// ProbeExifTool 验证 exiftool 可用并返回版本号。
// 先做路径查找，再实际调用，能区分「根本没装」和「装了但运行报错」。
func ProbeExifTool(ctx context.Context, bin string) (string, error) {
	if _, err := exec.LookPath(bin); err != nil {
		return "", fmt.Errorf("找不到可执行文件 %q: %w", bin, err)
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, bin, "-ver").Output()
	if err != nil {
		return "", fmt.Errorf("%s 执行失败: %w", bin, err)
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return "", fmt.Errorf("%s 版本输出为空", bin)
	}
	return v, nil
}

// libheifVersionRe 从 heif-convert 的 usage 输出里提取 libheif 版本。
var libheifVersionRe = regexp.MustCompile(`libheif version:\s*([0-9]+(?:\.[0-9]+)+)`)

// ProbeHeifConvert 验证 heif-convert 可用并尽量返回版本号。
// heif-convert 不支持 --version（多数版本会回 usage 到 stderr），
// 因此从输出里提取 libheif 版本；提取不到时只报告「已就绪」。
func ProbeHeifConvert(ctx context.Context, bin string) (string, error) {
	if _, err := exec.LookPath(bin); err != nil {
		return "", fmt.Errorf("找不到可执行文件 %q: %w", bin, err)
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(cctx, bin, "--version").CombinedOutput()
	s := string(out)
	if m := libheifVersionRe.FindStringSubmatch(s); len(m) > 1 {
		return "libheif " + m[1], nil
	}
	v := strings.TrimSpace(s)
	if i := strings.IndexByte(v, '\n'); i > 0 {
		v = v[:i]
	}
	low := strings.ToLower(v)
	if v == "" || strings.Contains(low, "usage") || strings.Contains(low, "error") ||
		strings.Contains(low, "unrecognized") || strings.Contains(low, "invalid") {
		return "已就绪", nil
	}
	return v, nil
}
