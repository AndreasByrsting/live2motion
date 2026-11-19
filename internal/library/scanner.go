package library

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AndreasByrsting/live2motion/internal/config"
	"github.com/AndreasByrsting/live2motion/internal/store"
)

// Logger 由 engine 注入，负责落库并广播到 Web。
type Logger interface {
	Info(scope, format string, args ...any)
	Warn(scope, format string, args ...any)
	Error(scope, format string, args ...any)
}

// skipDirNames 明确跳过的目录：群晖缩略图、回收站、同步工具元数据等。
var skipDirNames = map[string]struct{}{
	"@eaDir":      {},
	"#recycle":    {},
	"#snapshot":   {},
	"@Recycle":    {},
	".stfolder":   {},
	".stversions": {},
	"lost+found":  {},
}

// ShouldSkipDir 判断目录名是否应跳过：隐藏目录、群晖缩略图、回收站、同步工具元数据等。
// 扫描与「完整图库模式」的镜像复制共用同一套规则，保证两边看到的文件集合一致。
func ShouldSkipDir(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.HasPrefix(name, ".") {
		return true
	}
	_, ok := skipDirNames[name]
	return ok
}

// ShouldSkipFileName 判断文件名是否应跳过：隐藏文件、同步工具/编辑器临时文件。
// 这些文件不属于图库内容，复制过去只会给同步端添乱。
func ShouldSkipFileName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return true
	}
	if strings.HasPrefix(name, ".") {
		return true
	}
	lower := strings.ToLower(name)
	if strings.Contains(lower, "~syncthing~") {
		return true
	}
	for _, suf := range []string{".tmp", ".part", ".partial", ".swp", ".crdownload"} {
		if strings.HasSuffix(lower, suf) {
			return true
		}
	}
	return false
}

// Scanner 负责递归扫描源目录、读取 ContentIdentifier、增量维护 MD5 并完成配对。
type Scanner struct {
	st      *store.Store
	exifBin string
	log     Logger
}

// NewScanner 构造扫描器。
func NewScanner(st *store.Store, exifBin string, log Logger) *Scanner {
	return &Scanner{st: st, exifBin: exifBin, log: log}
}

// ScanOptions 一次扫描的输入。
type ScanOptions struct {
	SourceRoot      string
	Settings        config.Settings
	Trigger         string // manual / auto / startup / cli
	ForceDeepVerify bool   // 忽略 mtime 强制重算 MD5（人工触发完整性校验）

	// Progress 扫描过程中的实时进度回调（可选）。
	// 大图库首轮扫描可能持续数分钟，界面需要看到「正在做什么、已经走过多少文件」，
	// 否则统计全是 0，看起来像卡死。回调会在遍历、读元数据、算 MD5、配对各阶段被调用。
	Progress func(ScanProgress)
}

// 扫描阶段，供界面显示当前在做什么。
const (
	ScanPhaseWalk      = "walk"       // 遍历目录、登记文件
	ScanPhaseContentID = "content_id" // 读取 ContentIdentifier
	ScanPhaseMD5       = "md5"        // 计算 MD5
	ScanPhasePair      = "pair"       // 配对、创建任务
)

// ScanProgress 一次扫描的实时进度快照。
type ScanProgress struct {
	Phase     string // walk / content_id / md5 / pair
	FilesSeen int64  // 已发现的候选文件（HEIC + 视频）
	HeicSeen  int64
	VideoSeen int64
	Done      int64 // 当前阶段已处理数量（MD5 / 元数据读取时有效）
	Total     int64 // 当前阶段总量（0 表示未知）
	Elapsed   time.Duration
}

// ScanResult 一次扫描的输出统计。
type ScanResult struct {
	ScanID        int64
	FilesSeen     int64
	HeicSeen      int64
	VideoSeen     int64
	PairsTotal    int64
	PairsNew      int64
	PairsChanged  int64
	PairsUnpaired int64
	Warnings      int64
	Duration      time.Duration
	PairStats     PairStats
}

// progressReporter 把扫描进度节流后回调给上层。
// 遍历几万个文件时逐个回调会把 SSE 与模板刷新打爆，这里限流到 5 次/秒，
// 阶段切换与扫描结束时强制上报，保证界面不会停在旧阶段。
type progressReporter struct {
	mu     sync.Mutex
	fn     func(ScanProgress)
	began  time.Time
	last   time.Time
	minGap time.Duration
	p      ScanProgress
}

func newProgressReporter(fn func(ScanProgress), began time.Time) *progressReporter {
	if fn == nil {
		return nil
	}
	return &progressReporter{
		fn:     fn,
		began:  began,
		minGap: 200 * time.Millisecond,
		p:      ScanProgress{Phase: ScanPhaseWalk},
	}
}

func (r *progressReporter) update(force bool, mutate func(p *ScanProgress)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	mutate(&r.p)
	r.p.Elapsed = time.Since(r.began)
	now := time.Now()
	if !force && now.Sub(r.last) < r.minGap {
		r.mu.Unlock()
		return
	}
	r.last = now
	snap := r.p
	r.mu.Unlock()
	r.fn(snap)
}

// setPhase 切换阶段（强制上报，界面能立刻看到「正在读元数据」之类）。
func (r *progressReporter) setPhase(phase string, total int64) {
	r.update(true, func(p *ScanProgress) {
		p.Phase = phase
		p.Done = 0
		p.Total = total
	})
}

// step 当前阶段完成一项。
func (r *progressReporter) step() {
	r.update(false, func(p *ScanProgress) { p.Done++ })
}

// flush 结束前强制上报一次，确保 Done 追平 Total。
func (r *progressReporter) flush() { r.update(true, func(*ScanProgress) {}) }

// scannedFile 扫描过程中单个文件的上下文。
type scannedFile struct {
	asset   *store.Asset
	abs     string
	prev    store.Asset
	created bool
	noRead  bool // 0 字节或不可读，跳过解析
}

const upsertBatch = 400

// Scan 执行一次完整扫描。返回的 error 只表示致命错误（目录不可读、数据库不可用等），
// 单文件级别的失败会记入 Warnings 与日志。
func (s *Scanner) Scan(ctx context.Context, opt ScanOptions) (*ScanResult, error) {
	root := filepath.Clean(opt.SourceRoot)
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("源目录不可访问 %s: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("源路径不是目录: %s", root)
	}

	scanID, err := s.st.StartScan(ctx, root, opt.Trigger)
	if err != nil {
		return nil, fmt.Errorf("创建扫描批次失败: %w", err)
	}
	res := &ScanResult{ScanID: scanID}
	began := time.Now()
	progress := newProgressReporter(opt.Progress, began)
	progress.update(true, func(*ScanProgress) {})

	// exiftool 版本变化时，让上一轮「读不到 ContentIdentifier」的文件重新解析。
	// 否则升级工具后旧记录仍被当成普通文件，永远配不上对。
	s.invalidateStaleCIDCache(ctx)

	files, walkErr := s.collect(ctx, root, scanID, opt, res, progress)
	if walkErr != nil {
		s.st.FinishScan(ctx, store.ScanRun{
			ID: scanID, SourceRoot: root, Trigger: opt.Trigger,
			FilesSeen: res.FilesSeen, HeicSeen: res.HeicSeen, VideoSeen: res.VideoSeen,
			Warnings: res.Warnings, Error: walkErr.Error(),
		})
		return res, walkErr
	}

	s.resolveContentIDs(ctx, files, res, progress)
	s.computeMD5s(ctx, files, opt, res, progress)
	pairStats, err := s.buildPairs(ctx, root, opt, res, progress)
	if err != nil {
		s.log.Warn("scan", "配对阶段异常: %v", err)
		res.Warnings++
	}

	// 扫描完整成功后才把本轮未见到的资产标记 missing，避免误判。
	if missing, err := s.st.MarkMissingAssets(ctx, root, scanID); err != nil {
		s.log.Warn("scan", "标记缺失文件失败: %v", err)
	} else if missing > 0 {
		s.log.Info("scan", "本轮有 %d 个源文件已消失，标记为 missing", missing)
	}
	// 源文件已移除的任务再重试只会失败，每轮都顺手标成跳过（幂等，代价是一条 UPDATE）
	if n, err := s.st.SkipJobsForMissingAssets(ctx, root); err != nil {
		s.log.Warn("scan", "跳过失效任务失败: %v", err)
	} else if n > 0 {
		s.log.Info("scan", "已将 %d 个源文件已移除的任务标记为跳过", n)
	}

	res.Duration = time.Since(began)
	progress.flush()
	if total, err := s.st.CountPairs(ctx, root); err == nil {
		res.PairsTotal = total
	}
	_ = pairStats

	if err := s.st.FinishScan(ctx, store.ScanRun{
		ID: res.ScanID, SourceRoot: root, Trigger: opt.Trigger,
		FilesSeen: res.FilesSeen, HeicSeen: res.HeicSeen, VideoSeen: res.VideoSeen,
		PairsTotal: res.PairsTotal, PairsNew: res.PairsNew, PairsUnpaired: res.PairsUnpaired,
		Warnings: res.Warnings,
	}); err != nil {
		s.log.Warn("scan", "回写扫描结果失败: %v", err)
	}
	return res, nil
}

// invalidateStaleCIDCache 在 exiftool 版本变化时清空「无 CID」的解析缓存。
// 只在版本变化时触发一次，避免每轮扫描都重读大量普通照片。
func (s *Scanner) invalidateStaleCIDCache(ctx context.Context) {
	ver, err := ProbeExifTool(ctx, s.exifBin)
	if err != nil {
		return
	}
	prev, err := s.st.GetMeta(ctx, store.MetaExifToolVersion)
	if err != nil {
		return
	}
	if prev == ver {
		return
	}
	if n, rerr := s.st.ResetNoCIDAssets(ctx); rerr != nil {
		s.log.Warn("scan", "重置元数据解析缓存失败: %v", rerr)
	} else if n > 0 {
		old := prev
		if old == "" {
			old = "未知"
		}
		s.log.Warn("scan", "exiftool 版本变化（%s → %s），%d 个此前未识别元数据的文件将重新解析", old, ver, n)
	}
	if err := s.st.SetMeta(ctx, store.MetaExifToolVersion, ver); err != nil {
		s.log.Warn("scan", "记录 exiftool 版本失败: %v", err)
	}
}

// collect 递归遍历并批量登记源文件。
func (s *Scanner) collect(ctx context.Context, root string, scanID int64, opt ScanOptions, res *ScanResult, pr *progressReporter) ([]scannedFile, error) {
	files := make([]scannedFile, 0, 1024)
	buf := make([]*store.Asset, 0, upsertBatch)

	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		results, err := s.st.UpsertAssets(ctx, buf)
		if err != nil {
			return err
		}
		for _, r := range results {
			abs := filepath.Join(root, filepath.FromSlash(r.Asset.RelPath))
			sf := scannedFile{asset: r.Asset, abs: abs, prev: r.Prev, created: r.Created}
			if !r.Created {
				// 继承上一轮解析结果，避免未变文件被当成"无 CID"。
				r.Asset.ContentID = r.Prev.ContentID
				r.Asset.CIDSource = r.Prev.CIDSource
				r.Asset.MD5 = r.Prev.MD5
				r.Asset.MD5MtimeUnix = r.Prev.MD5MtimeUnix
				r.Asset.MD5CheckedAt = r.Prev.MD5CheckedAt
				if r.Asset.State == store.AssetStateOK {
					r.Asset.State = r.Prev.State
				}
			}
			if r.Asset.SizeBytes == 0 {
				sf.noRead = true
			}
			files = append(files, sf)
		}
		buf = buf[:0]
		return nil
	}

	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			s.log.Warn("scan", "无法访问 %s: %v", p, err)
			res.Warnings++
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p != root && ShouldSkipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		var kind string
		switch {
		case opt.Settings.HasHeicExt(ext):
			kind = store.KindHEIC
		case opt.Settings.HasVideoExt(ext):
			kind = store.KindVideo
		default:
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			s.log.Warn("scan", "读取文件信息失败 %s: %v", p, err)
			res.Warnings++
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		state := store.AssetStateOK
		if fi.Size() == 0 {
			state = store.AssetStateUnreadable
			s.log.Warn("scan", "跳过 0 字节文件: %s", p)
			res.Warnings++
		}
		buf = append(buf, &store.Asset{
			SourceRoot: root,
			RelPath:    filepath.ToSlash(rel),
			Kind:       kind,
			SizeBytes:  fi.Size(),
			MtimeUnix:  fi.ModTime().Unix(),
			State:      state,
			LastScanID: scanID,
		})
		res.FilesSeen++
		if kind == store.KindHEIC {
			res.HeicSeen++
		} else {
			res.VideoSeen++
		}
		seen, heics, videos := res.FilesSeen, res.HeicSeen, res.VideoSeen
		pr.update(false, func(p *ScanProgress) {
			p.Phase = ScanPhaseWalk
			p.FilesSeen, p.HeicSeen, p.VideoSeen = seen, heics, videos
		})
		if len(buf) >= upsertBatch {
			return flush()
		}
		return nil
	})
	if walkErr != nil {
		return files, walkErr
	}
	if err := flush(); err != nil {
		return files, err
	}
	return files, nil
}

// resolveContentIDs 批量读取 ContentIdentifier 并落库。
func (s *Scanner) resolveContentIDs(ctx context.Context, files []scannedFile, res *ScanResult, pr *progressReporter) {
	var need []int
	for i := range files {
		f := &files[i]
		if f.noRead {
			continue
		}
		switch {
		case f.created:
			need = append(need, i)
		case f.prev.MtimeUnix != f.asset.MtimeUnix:
			need = append(need, i)
		case f.prev.ContentID == "" && f.prev.State != store.AssetStateNoCID:
			// 没拿到 UUID 且不是「已确认的普通照片」→ 必须重试。
			// 覆盖两种历史脏状态：上次解析失败(unreadable)、以及旧版本写入的
			// state=ok 但 content_id 为空的记录（工具升级后这类记录会永久卡死，
			// 既不重读也不参与配对，表现为「明明是一对却永远配不上」）。
			need = append(need, i)
		}
	}
	if len(need) == 0 {
		return
	}

	paths := make([]string, 0, len(need))
	for _, i := range need {
		paths = append(paths, files[i].abs)
	}
	pr.setPhase(ScanPhaseContentID, int64(len(paths)))
	results, err := ReadContentIdentifiers(ctx, s.exifBin, paths)
	if err != nil {
		s.log.Error("scan", "批量读取 ContentIdentifier 失败: %v", err)
		res.Warnings++
		pr.update(true, func(p *ScanProgress) { p.Done = p.Total })
		return
	}
	pr.update(true, func(p *ScanProgress) { p.Done = p.Total })

	updates := make([]store.ContentIDUpdate, 0, len(need))
	var noCID, unreadable int
	for _, i := range need {
		f := &files[i]
		r, ok := results[f.abs]
		upd := store.ContentIDUpdate{ID: f.asset.ID}
		switch {
		case !ok:
			upd.State = store.AssetStateUnreadable
			upd.Warning = "exiftool 未返回该文件记录"
		case r.Err != "":
			upd.State = store.AssetStateUnreadable
			upd.Warning = "读取元数据失败: " + r.Err
		case r.ContentID == "":
			upd.State = store.AssetStateNoCID
			upd.Warning = "缺少 ContentIdentifier，按普通文件处理"
		default:
			upd.ContentID = r.ContentID
			upd.Source = r.Source
			upd.State = store.AssetStateOK
		}
		// 保留已有的 bit rot 告警，不被正常状态覆盖
		if f.prev.State == store.AssetStateBitrot && upd.State == store.AssetStateOK {
			upd.State = store.AssetStateBitrot
			upd.Warning = f.prev.Warning
		}
		f.asset.ContentID = upd.ContentID
		f.asset.CIDSource = upd.Source
		f.asset.State = upd.State
		f.asset.Warning = upd.Warning
		updates = append(updates, upd)

		switch upd.State {
		case store.AssetStateNoCID:
			noCID++
			// 普通照片可能成千上万，采样记录，避免日志表被刷爆
			if noCID <= 3 || noCID%200 == 0 {
				s.log.Warn("scan", "%s 缺少 ContentIdentifier，跳过配对", f.asset.RelPath)
			}
		case store.AssetStateUnreadable:
			unreadable++
			res.Warnings++
			if unreadable <= 20 {
				s.log.Warn("scan", "%s 元数据读取失败: %s", f.asset.RelPath, upd.Warning)
			}
		}
	}
	if noCID > 0 {
		s.log.Info("scan", "本轮共 %d 个文件缺少 ContentIdentifier（普通照片/视频），不参与配对", noCID)
	}
	if unreadable > 20 {
		s.log.Warn("scan", "本轮共 %d 个文件元数据读取失败（仅显示前 20 条）", unreadable)
	}
	if err := s.st.BulkSetContentID(ctx, updates); err != nil {
		s.log.Error("scan", "写入 ContentIdentifier 失败: %v", err)
		res.Warnings++
	}
}

// computeMD5s 增量维护 MD5：
//   - mtime 未变 → 复用上次结果，不读盘；
//   - mtime 变化 → 重新计算；
//   - 深度校验（周期或人工触发）→ 强制重算，若 mtime 未变而 MD5 变了，判定 bit rot。
func (s *Scanner) computeMD5s(ctx context.Context, files []scannedFile, opt ScanOptions, res *ScanResult, pr *progressReporter) {
	pr.setPhase(ScanPhaseMD5, 0)
	if !opt.Settings.EnableMD5 {
		return
	}
	now := time.Now().Unix()
	deepInterval := int64(opt.Settings.DeepVerifyHours) * 3600

	type task struct {
		idx   int
		force bool
	}
	var tasks []task
	for i := range files {
		f := &files[i]
		if f.noRead {
			continue
		}
		due := opt.ForceDeepVerify
		if !due && deepInterval > 0 && f.prev.MD5 != "" && now-f.prev.MD5CheckedAt >= deepInterval {
			due = true
		}
		if !due && f.prev.MD5 != "" && f.prev.MD5MtimeUnix == f.asset.MtimeUnix {
			f.asset.MD5 = f.prev.MD5
			f.asset.MD5MtimeUnix = f.prev.MD5MtimeUnix
			continue
		}
		tasks = append(tasks, task{idx: i, force: due})
	}
	if len(tasks) == 0 {
		return
	}
	pr.setPhase(ScanPhaseMD5, int64(len(tasks)))

	conc := opt.Settings.Workers
	if conc < 1 {
		conc = 1
	}
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	var mu sync.Mutex
	updates := make([]store.MD5Update, 0, len(tasks))
	var events []store.IntegrityEvent
	states := make([]store.StateUpdate, 0, 4)

	for _, t := range tasks {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(t task) {
			defer wg.Done()
			defer func() { <-sem }()
			defer pr.step()
			f := &files[t.idx]
			sum, n, err := MD5File(f.abs)
			if err != nil {
				mu.Lock()
				res.Warnings++
				states = append(states, store.StateUpdate{
					ID: f.asset.ID, State: store.AssetStateUnreadable,
					Warning: "计算 MD5 失败: " + err.Error(),
				})
				mu.Unlock()
				s.log.Warn("hash", "计算 MD5 失败 %s: %v", f.asset.RelPath, err)
				return
			}
			if n != f.asset.SizeBytes {
				s.log.Warn("hash", "%s 读取字节数(%d)与文件大小(%d)不一致，可能正在写入",
					f.asset.RelPath, n, f.asset.SizeBytes)
			}
			mu.Lock()
			defer mu.Unlock()
			updates = append(updates, store.MD5Update{
				ID: f.asset.ID, MD5: sum, MtimeUnix: f.asset.MtimeUnix,
			})
			f.asset.MD5 = sum
			f.asset.MD5MtimeUnix = f.asset.MtimeUnix
			f.asset.MD5CheckedAt = now

			if f.prev.MD5 != "" && f.prev.MD5 != sum && f.prev.MD5MtimeUnix == f.asset.MtimeUnix {
				// mtime 与上次一致说明文件本身没被正常修改，但内容变了 → 静默损坏
				events = append(events, store.IntegrityEvent{
					AssetID: f.asset.ID, RelPath: f.asset.RelPath, Kind: f.asset.Kind,
					OldMD5: f.prev.MD5, NewMD5: sum,
					Detail: "mtime 未变化但 MD5 改变",
				})
				states = append(states, store.StateUpdate{
					ID: f.asset.ID, State: store.AssetStateBitrot,
					Warning: "疑似静默损坏：mtime 未变但 MD5 改变",
				})
			}
		}(t)
	}
	wg.Wait()
	pr.flush()

	if err := s.st.BulkSetMD5(ctx, updates); err != nil {
		s.log.Error("hash", "写入 MD5 失败: %v", err)
		res.Warnings++
	}
	if err := s.st.BulkSetState(ctx, states); err != nil {
		s.log.Error("hash", "更新完整性状态失败: %v", err)
	}
	for _, e := range events {
		if err := s.st.AddIntegrityEvent(ctx, e); err != nil {
			s.log.Warn("hash", "记录完整性事件失败: %v", err)
		}
		s.log.Error("hash", "检测到疑似静默损坏: %s (旧 %s → 新 %s)", e.RelPath, e.OldMD5, e.NewMD5)
	}
}

// buildPairs 用 ContentIdentifier 配对并创建任务。
func (s *Scanner) buildPairs(ctx context.Context, root string, opt ScanOptions, res *ScanResult, pr *progressReporter) (PairStats, error) {
	cands, err := s.st.ListPairingCandidates(ctx, root)
	if err != nil {
		return PairStats{}, err
	}
	pairs, stats := BuildPairs(cands)
	res.PairsUnpaired = int64(stats.OrphanHeic + stats.OrphanVideo)
	pr.setPhase(ScanPhasePair, int64(len(pairs)))

	maxAttempts := opt.Settings.MaxRetries + 1
	for _, pc := range pairs {
		pr.step()
		p := &store.Pair{
			SourceRoot:   root,
			ContentID:    pc.ContentID,
			HeicAssetID:  pc.Heic.ID,
			VideoAssetID: pc.Video.ID,
			HeicRelPath:  pc.Heic.RelPath,
			VideoRelPath: pc.Video.RelPath,
			HeicMtime:    pc.Heic.MtimeUnix,
			VideoMtime:   pc.Video.MtimeUnix,
			HeicSize:     pc.Heic.SizeBytes,
			VideoSize:    pc.Video.SizeBytes,
		}
		pairID, created, changed, err := s.st.UpsertPair(ctx, p)
		if err != nil {
			s.log.Warn("pair", "登记配对失败 %s: %v", pc.ContentID, err)
			res.Warnings++
			continue
		}
		if created {
			res.PairsNew++
		}
		if changed {
			res.PairsChanged++
		}
		if _, _, err := s.st.EnsureJob(ctx, pairID, maxAttempts); err != nil {
			s.log.Warn("pair", "创建任务失败 %s: %v", pc.ContentID, err)
			res.Warnings++
			continue
		}
		if changed && opt.Settings.ReconvertOnChange {
			if n, err := s.st.ResetJobForChange(ctx, pairID); err != nil {
				s.log.Warn("pair", "重置任务失败 %s: %v", pc.ContentID, err)
			} else if n > 0 {
				s.log.Info("pair", "源文件已变化，重新排队: %s", pc.Heic.RelPath)
			}
		}
	}

	if stats.OrphanHeic > 0 || stats.OrphanVideo > 0 {
		s.log.Warn("pair", "存在未配对文件：缺失视频的 HEIC %d 个，缺失 HEIC 的视频 %d 个",
			stats.OrphanHeic, stats.OrphanVideo)
	}
	if stats.CrossFolder > 0 {
		s.log.Info("pair", "有 %d 组配对跨越目录（同目录优先规则之外）", stats.CrossFolder)
	}
	return stats, nil
}

// SortedCIDs 便于测试与调试，返回配对结果中出现的 UUID 列表（去重排序）。
func SortedCIDs(pairs []PairCandidate) []string {
	seen := make(map[string]struct{}, len(pairs))
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		if _, ok := seen[p.ContentID]; ok {
			continue
		}
		seen[p.ContentID] = struct{}{}
		out = append(out, p.ContentID)
	}
	sort.Strings(out)
	return out
}
