// Package mirror 负责「完整图库模式」：把不参与转换的源文件按原目录结构
// 原样复制到输出目录，并清理那些「已经转换成功、不再需要原件」的副本。
//
// 输出目录的最终形态因此是：
//
//	<output>/2024/05/IMG_0001-motion.jpg   ← 转换产物（HEIC+MOV 合成，原件留在源目录）
//	<output>/2024/05/IMG_0002.HEIC         ← 普通照片，原样复制
//	<output>/2024/06/VID_0001.MOV          ← 没配上对的视频，原样复制
//
// 目录结构与源目录完全一致，所以 Syncthing 同步过去之后，Pixel 上既完整又不重复。
//
// # 只写终态（重要）
//
// 输出目录一旦写出文件，下游（Syncthing → Immich / 手机）会立刻取走上传；
// 之后再删除并不能撤回已经上传的那一份。所以写入必须「白名单化」：
//
//	非转换类扩展名（JPG/PNG/DNG/AAE…）→ 永远不会变成 Motion Photo，直接复制；
//	转换类扩展名（HEIC/HEIF/MOV/MP4…）→ 只有在 Options.Allow 里才允许复制。
//
// Allow 由引擎按「这组实况已定型且不会转换」计算（转换失败超过观察期、
// 或从未配对且已过观察期）。其余情况一律按住不写，等定型后下一轮再判断。
package mirror

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/immich2motion/immich2motion/internal/library"
	"github.com/immich2motion/immich2motion/internal/store"
)

// Logger 与 library.Logger 一致，由 engine 注入，落库并广播到 Web。
type Logger interface {
	Info(scope, format string, args ...any)
	Warn(scope, format string, args ...any)
	Error(scope, format string, args ...any)
}

// Syncer 执行未转换文件的镜像复制与遗留副本清理。
type Syncer struct {
	st  *store.Store
	log Logger
}

// New 构造镜像同步器。
func New(st *store.Store, log Logger) *Syncer {
	return &Syncer{st: st, log: log}
}

// Options 一次镜像同步的输入。
type Options struct {
	SourceDir string
	OutputDir string

	// Allow 允许以「原样副本」形式写入输出目录的转换类源文件（相对路径，统一用 / 分隔）。
	// 不在集合里的转换类文件一律不写：它们还没定型，写了之后很可能又要删，
	// 而删除无法撤回上游已经同步走的那一份。
	Allow map[string]struct{}

	// ConvertibleExts 参与转换的扩展名（小写、带点，如 ".heic"）。
	// 这些扩展名的文件受 Allow 管控；其它扩展名永远不会被合成成 Motion Photo，可以立即复制。
	ConvertibleExts map[string]struct{}
}

// Stats 一次镜像同步的统计。
type Stats struct {
	Seen    int64 // 遍历到的文件数（不含被跳过的临时文件与目录）
	Copied  int64 // 新复制
	Updated int64 // 源文件变化后重新复制
	Skipped int64 // 已是最新，未读盘
	Held    int64 // 观察期内故意没复制（等定型）
	Pruned  int64 // 清理掉的副本
	Failed  int64
	Bytes   int64
}

const (
	batchSize  = 200     // 每累积多少条记录提交一次
	copyBufLen = 1 << 20 // 1 MB 复制缓冲
	tmpPattern = ".i2m-*.tmp"
)

// Run 执行一次镜像同步：遍历源目录，把「允许写入」的文件复制到输出目录，
// 并清理本轮不该再留的副本（源文件已消失 / 已转换成功 / 退回观察期）。
func (s *Syncer) Run(ctx context.Context, opt Options) (Stats, error) {
	var stats Stats
	root := filepath.Clean(opt.SourceDir)
	out := filepath.Clean(opt.OutputDir)

	if root == "" || root == "." || out == "" || out == "." {
		return stats, fmt.Errorf("目录配置无效（源 %q / 输出 %q）", opt.SourceDir, opt.OutputDir)
	}
	info, err := os.Stat(root)
	if err != nil {
		return stats, fmt.Errorf("源目录不可访问 %s: %w", root, err)
	}
	if !info.IsDir() {
		return stats, fmt.Errorf("源路径不是目录: %s", root)
	}
	// 输出目录落在源目录内会让镜像去复制自己的产物（无限套娃），直接拒绝
	if err := ensureOutside(out, root); err != nil {
		return stats, err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return stats, fmt.Errorf("创建输出目录失败: %w", err)
	}

	records, err := s.st.ListMirrorRecords(ctx, root)
	if err != nil {
		return stats, fmt.Errorf("读取镜像记录失败: %w", err)
	}
	// keep 记录本轮「仍然应该留在输出目录」的副本。遍历结束后，
	// 不在 keep 里的记录一律清理——这同时覆盖三种情况：
	//   源文件已删除/改名、源文件已转换成功、源文件退回观察期（不该再有副本）。
	keep := make(map[string]struct{}, len(records))

	var batch []store.MirrorRecord
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := s.st.UpsertMirrorRecords(ctx, root, batch); err != nil {
			return err
		}
		batch = batch[:0]
		return nil
	}

	// walkBroken：遍历过程中出现过无法访问的条目时，不再按「未见到」删副本。
	// 权限临时抖动不应该被当成「源文件已删除」，否则会误删输出目录里的有效文件。
	var walkBroken bool

	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if walkErr != nil {
			s.log.Warn("mirror", "无法访问 %s: %v", p, walkErr)
			stats.Failed++
			walkBroken = true
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p != root && library.ShouldSkipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if library.ShouldSkipFileName(d.Name()) {
			return nil
		}

		relOS, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		rel := filepath.ToSlash(relOS)
		stats.Seen++

		// 白名单闸门：转换类文件只有「已定型、确定不会转换」时才允许落盘。
		// 这里只是不写，已有副本由遍历结束后的对账统一清理。
		if s.isConvertible(d.Name(), opt) {
			if _, ok := opt.Allow[rel]; !ok {
				stats.Held++
				return nil
			}
		}
		keep[rel] = struct{}{}

		fi, err := d.Info()
		if err != nil {
			s.log.Warn("mirror", "读取文件信息失败 %s: %v", rel, err)
			stats.Failed++
			return nil
		}

		dst := filepath.Join(out, filepath.FromSlash(rel))
		if err := ensureInside(dst, out); err != nil {
			s.log.Warn("mirror", "跳过越界路径 %s: %v", rel, err)
			stats.Failed++
			return nil
		}

		_, tracked := records[rel]
		if tracked && sameFingerprint(records[rel], fi) {
			if _, err := os.Stat(dst); err == nil {
				stats.Skipped++
				return nil
			}
			// 记录还在但副本被删了（用户手工清理过）→ 重新复制
		}

		n, err := copyFile(ctx, p, dst, fi.ModTime())
		if err != nil {
			s.log.Warn("mirror", "复制失败 %s: %v", rel, err)
			stats.Failed++
			return nil
		}
		if tracked {
			stats.Updated++
		} else {
			stats.Copied++
		}
		stats.Bytes += n
		batch = append(batch, store.MirrorRecord{
			RelPath: rel, SizeBytes: fi.Size(), MtimeUnix: fi.ModTime().Unix(),
		})
		if len(batch) >= batchSize {
			if err := flush(); err != nil {
				return fmt.Errorf("写入镜像记录失败: %w", err)
			}
		}
		return nil
	})
	// 取消或遍历失败时立刻返回：此时 seen 集合不完整，绝不能拿去删副本。
	// 但已经复制出去的文件必须登记，否则下一轮会把它们当成新文件再复制一遍。
	if err := ctx.Err(); err != nil {
		s.flushDetached(root, batch)
		return stats, err
	}
	if walkErr != nil {
		s.flushDetached(root, batch)
		return stats, walkErr
	}
	if err := flush(); err != nil {
		return stats, err
	}

	if walkBroken {
		if len(records) > len(keep) {
			s.log.Warn("mirror", "遍历存在访问错误，本轮跳过副本对账（%d 条记录待下轮确认）", len(records)-len(keep))
		}
		return stats, nil
	}
	gone := make([]string, 0, len(records))
	for rel := range records {
		if _, ok := keep[rel]; !ok {
			gone = append(gone, rel)
		}
	}
	if len(gone) > 0 {
		n, err := s.pruneMany(ctx, root, out, gone)
		stats.Pruned += n
		if err != nil {
			s.log.Warn("mirror", "清理失效副本失败: %v", err)
			stats.Failed++
		}
	}
	return stats, nil
}

// isConvertible 判断该文件是否会参与转换（只看扩展名，与扫描器的收录规则一致）。
func (s *Syncer) isConvertible(name string, opt Options) bool {
	if len(opt.ConvertibleExts) == 0 {
		return false
	}
	_, ok := opt.ConvertibleExts[strings.ToLower(filepath.Ext(name))]
	return ok
}

// PruneSources 清理这些源文件在输出目录里可能存在的镜像副本。
// 转换成功后调用：原件已经合成进 JPG，再同步一份 HEIC/MOV 过去就是重复。
func (s *Syncer) PruneSources(ctx context.Context, sourceDir, outputDir string, rels []string) (int64, error) {
	root := filepath.Clean(sourceDir)
	out := filepath.Clean(outputDir)
	if root == "" || root == "." || out == "" || out == "." {
		return 0, nil
	}
	var n int64
	for _, rel := range rels {
		if strings.TrimSpace(rel) == "" {
			continue
		}
		got, err := s.pruneOne(ctx, root, out, filepath.ToSlash(rel))
		n += got
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// pruneOne 删除单个镜像副本。只有存在记录才动手——输出目录里用户自己放的文件不能碰。
func (s *Syncer) pruneOne(ctx context.Context, root, out, rel string) (int64, error) {
	if _, ok, err := s.st.GetMirrorRecord(ctx, root, rel); err != nil {
		return 0, err
	} else if !ok {
		return 0, nil
	}
	dst := filepath.Join(out, filepath.FromSlash(rel))
	if err := ensureInside(dst, out); err != nil {
		return 0, err
	}
	// 先删文件再删记录：中途崩溃最多留下一条可以被下一轮修复的记录，
	// 反过来则会留下一个永远无人认领的文件。
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	if err := s.st.DeleteMirrorRecord(ctx, root, rel); err != nil {
		return 0, err
	}
	return 1, nil
}

// pruneMany 批量清理副本，返回清理数量。
func (s *Syncer) pruneMany(ctx context.Context, root, out string, rels []string) (int64, error) {
	var (
		n       int64
		removed []string
	)
	for _, rel := range rels {
		dst := filepath.Join(out, filepath.FromSlash(rel))
		if err := ensureInside(dst, out); err != nil {
			continue
		}
		if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
			s.log.Warn("mirror", "删除旧副本失败 %s: %v", rel, err)
			continue
		}
		removed = append(removed, rel)
		n++
	}
	if err := s.st.DeleteMirrorRecords(ctx, root, removed); err != nil {
		return n, err
	}
	return n, nil
}

// flushDetached 在上下文已取消时仍然把记录落库（用一个短超时的独立上下文）。
// 复制已经完成，记录丢了只会导致下一轮白复制一遍。
func (s *Syncer) flushDetached(root string, batch []store.MirrorRecord) {
	if len(batch) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.st.UpsertMirrorRecords(ctx, root, batch); err != nil {
		s.log.Warn("mirror", "写入镜像记录失败（%d 条）: %v", len(batch), err)
	}
}

// sameFingerprint 判断源文件是否与上次复制时一致（mtime + 大小）。
// 与扫描器的增量策略保持一致：不读盘、不算 MD5，宁可在极端情况下漏一次，也不每轮全量重算。
func sameFingerprint(rec store.MirrorRecord, fi os.FileInfo) bool {
	return rec.SizeBytes == fi.Size() && rec.MtimeUnix == fi.ModTime().Unix()
}

// copyFile 把 src 复制到 dst：同目录临时文件 → 校验写入长度 → 原子 rename。
// 保留源文件 mtime，Syncthing 两侧时间一致，增量判断也更直观。
func copyFile(ctx context.Context, src, dst string, mtime time.Time) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, fmt.Errorf("创建输出目录失败: %w", err)
	}

	in, err := os.Open(src) // 只读打开源文件
	if err != nil {
		return 0, err
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dst), tmpPattern)
	if err != nil {
		return 0, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // rename 成功后这里只是空操作

	buf := make([]byte, copyBufLen)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			tmp.Close()
			return total, err
		}
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := tmp.Write(buf[:n]); werr != nil {
				tmp.Close()
				return total, werr
			}
			total += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			tmp.Close()
			return total, rerr
		}
	}
	if err := tmp.Close(); err != nil {
		return total, err
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return total, err
	}
	if err := os.Chtimes(tmpPath, mtime, mtime); err != nil {
		return total, err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return total, err
	}
	return total, nil
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
		return fmt.Errorf("输出目录不能位于源目录内部: %s", target)
	}
	return nil
}
