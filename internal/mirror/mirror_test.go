package mirror_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/immich2motion/immich2motion/internal/mirror"
	"github.com/immich2motion/immich2motion/internal/store"
)

// nopLogger 让测试输出干净，同时验证 Logger 接口形状。
type nopLogger struct{}

func (nopLogger) Info(string, string, ...any)  {}
func (nopLogger) Warn(string, string, ...any)  {}
func (nopLogger) Error(string, string, ...any) {}

func newSyncer(t *testing.T) (*mirror.Syncer, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return mirror.New(st, nopLogger{}), st
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return string(b)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// 核心语义：只复制「不会参与转换」的文件，且相对路径与源目录完全一致。
func TestRunCopiesUnconvertedKeepingStructure(t *testing.T) {
	syncer, _ := newSyncer(t)
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	out := filepath.Join(dir, "output")

	// 已转换成功的实况：HEIC + MOV 都不该出现在输出目录
	writeFile(t, filepath.Join(src, "2024", "05", "IMG_0001.HEIC"), "heic-bytes")
	writeFile(t, filepath.Join(src, "2024", "05", "IMG_0001.MOV"), "mov-bytes")
	// 普通照片（没有 ContentIdentifier）：必须原样复制
	writeFile(t, filepath.Join(src, "2024", "05", "IMG_0002.HEIC"), "plain-photo")
	// 孤儿视频：必须原样复制
	writeFile(t, filepath.Join(src, "2024", "06", "VID_0001.MOV"), "orphan-video")
	// 非媒体文件也要复制（完整图库）
	writeFile(t, filepath.Join(src, "2024", "06", "IMG_0003.AAE"), "<aae/>")
	// 临时文件与隐藏目录：跳过
	writeFile(t, filepath.Join(src, "2024", "06", "photo.jpg.tmp"), "tmp")
	writeFile(t, filepath.Join(src, ".DS_Store"), "junk")
	writeFile(t, filepath.Join(src, "@eaDir", "thumb.jpg"), "synology")

	stamp := time.Unix(1700000000, 0)
	if err := os.Chtimes(filepath.Join(src, "2024", "05", "IMG_0002.HEIC"), stamp, stamp); err != nil {
		t.Fatal(err)
	}

	stats, err := syncer.Run(ctx, mirror.Options{
		SourceDir: src,
		OutputDir: out,
		Exclude: map[string]bool{
			"2024/05/IMG_0001.HEIC": true,
			"2024/05/IMG_0001.MOV":  true,
		},
	})
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if stats.Copied != 3 {
		t.Fatalf("应复制 3 个文件，实际 %+v", stats)
	}

	cases := map[string]string{
		"2024/05/IMG_0002.HEIC": "plain-photo",
		"2024/06/VID_0001.MOV":  "orphan-video",
		"2024/06/IMG_0003.AAE":  "<aae/>",
	}
	for rel, want := range cases {
		got := filepath.Join(out, filepath.FromSlash(rel))
		if !exists(got) {
			t.Fatalf("%s 应被复制到输出目录", rel)
		}
		if body := readFile(t, got); body != want {
			t.Fatalf("%s 内容不一致: %q", rel, body)
		}
		if filepath.Dir(got) == out {
			t.Fatalf("%s 不应被平铺在输出根目录", rel)
		}
	}

	// 已转换的实况原件一份都不能出现
	for _, rel := range []string{"2024/05/IMG_0001.HEIC", "2024/05/IMG_0001.MOV"} {
		if exists(filepath.Join(out, filepath.FromSlash(rel))) {
			t.Fatalf("%s 已合成进 JPG，不应再复制原件", rel)
		}
	}
	// 临时文件 / 隐藏目录不复制
	for _, rel := range []string{"2024/06/photo.jpg.tmp", ".DS_Store", "@eaDir/thumb.jpg"} {
		if exists(filepath.Join(out, filepath.FromSlash(rel))) {
			t.Fatalf("%s 属于临时文件或跳过目录，不应复制", rel)
		}
	}

	// mtime 必须保留：Syncthing 两侧时间一致，增量判断才直观
	fi, err := os.Stat(filepath.Join(out, "2024", "05", "IMG_0002.HEIC"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.ModTime().Unix() != stamp.Unix() {
		t.Fatalf("mtime 未保留: 期望 %d 实际 %d", stamp.Unix(), fi.ModTime().Unix())
	}
}

// 重复运行必须幂等：不重复读盘、不重复复制。
func TestRunIsIncremental(t *testing.T) {
	syncer, _ := newSyncer(t)
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	out := filepath.Join(dir, "output")
	writeFile(t, filepath.Join(src, "a", "IMG_0002.HEIC"), "v1")
	writeFile(t, filepath.Join(src, "a", "IMG_0003.HEIC"), "v1")

	opt := mirror.Options{SourceDir: src, OutputDir: out}
	if _, err := syncer.Run(ctx, opt); err != nil {
		t.Fatal(err)
	}

	stats, err := syncer.Run(ctx, opt)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Copied != 0 || stats.Updated != 0 || stats.Skipped != 2 {
		t.Fatalf("第二次运行应全部跳过，实际 %+v", stats)
	}

	// 源文件内容变化（mtime 也跟着变）→ 重新复制
	target := filepath.Join(src, "a", "IMG_0002.HEIC")
	if err := os.WriteFile(target, []byte("v2-longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(target, later, later); err != nil {
		t.Fatal(err)
	}
	stats, err = syncer.Run(ctx, opt)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Updated != 1 || stats.Copied != 0 {
		t.Fatalf("变化文件应计为更新，实际 %+v", stats)
	}
	if body := readFile(t, filepath.Join(out, "a", "IMG_0002.HEIC")); body != "v2-longer" {
		t.Fatalf("输出内容未更新: %q", body)
	}
}

// 转换成功后要清掉此前复制出去的原件，否则 Pixel 上就是重复的。
func TestRunPrunesCopiesAfterConversion(t *testing.T) {
	syncer, st := newSyncer(t)
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	out := filepath.Join(dir, "output")
	writeFile(t, filepath.Join(src, "a", "IMG_0001.HEIC"), "heic")
	writeFile(t, filepath.Join(src, "a", "IMG_0001.MOV"), "mov")

	// 第一次：转换还没成功（失败/未配对），原件会被复制
	if _, err := syncer.Run(ctx, mirror.Options{SourceDir: src, OutputDir: out}); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(out, "a", "IMG_0001.HEIC")) {
		t.Fatal("未转换时原件应被复制")
	}

	// 第二次：转换成功 → 原件副本必须消失
	stats, err := syncer.Run(ctx, mirror.Options{
		SourceDir: src, OutputDir: out,
		Exclude: map[string]bool{"a/IMG_0001.HEIC": true, "a/IMG_0001.MOV": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pruned != 2 {
		t.Fatalf("应清理 2 个副本，实际 %+v", stats)
	}
	for _, name := range []string{"IMG_0001.HEIC", "IMG_0001.MOV"} {
		if exists(filepath.Join(out, "a", name)) {
			t.Fatalf("%s 副本应被清理", name)
		}
	}
	if n, err := st.CountMirrors(ctx, src); err != nil || n != 0 {
		t.Fatalf("记录应被清空，实际 %d (%v)", n, err)
	}
}

// 源文件被删除 / 改名后，输出目录里对应的副本也要消失。
func TestRunRemovesCopyWhenSourceGone(t *testing.T) {
	syncer, st := newSyncer(t)
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	out := filepath.Join(dir, "output")
	writeFile(t, filepath.Join(src, "a", "IMG_0002.HEIC"), "x")
	writeFile(t, filepath.Join(src, "a", "IMG_0003.HEIC"), "y")

	opt := mirror.Options{SourceDir: src, OutputDir: out}
	if _, err := syncer.Run(ctx, opt); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(src, "a", "IMG_0003.HEIC")); err != nil {
		t.Fatal(err)
	}

	stats, err := syncer.Run(ctx, opt)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pruned != 1 {
		t.Fatalf("应清理 1 个副本，实际 %+v", stats)
	}
	if exists(filepath.Join(out, "a", "IMG_0003.HEIC")) {
		t.Fatal("源文件已删除，副本应一并删除")
	}
	if !exists(filepath.Join(out, "a", "IMG_0002.HEIC")) {
		t.Fatal("其它文件的副本不应受影响")
	}
	if n, _ := st.CountMirrors(ctx, src); n != 1 {
		t.Fatalf("应剩 1 条记录，实际 %d", n)
	}
}

// PruneSources 只删自己复制过的文件：用户在输出目录里手工放的东西必须安全。
func TestPruneSourcesOnlyTouchesTrackedFiles(t *testing.T) {
	syncer, _ := newSyncer(t)
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	out := filepath.Join(dir, "output")
	writeFile(t, filepath.Join(src, "a", "IMG_0001.HEIC"), "src")

	// 用户自己的文件，位置刚好与某个源文件路径相同，但没有复制记录
	writeFile(t, filepath.Join(out, "a", "IMG_0001.HEIC"), "user-own")

	n, err := syncer.PruneSources(ctx, src, out, []string{"a/IMG_0001.HEIC"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("没有记录时不应删除任何文件，实际 %d", n)
	}
	if body := readFile(t, filepath.Join(out, "a", "IMG_0001.HEIC")); body != "user-own" {
		t.Fatalf("用户文件被误删/误改: %q", body)
	}
}

// 输出目录落在源目录内部时必须拒绝（否则会复制自己的产物）。
func TestRunRejectsOutputInsideSource(t *testing.T) {
	syncer, _ := newSyncer(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	writeFile(t, filepath.Join(src, "a", "IMG_0002.HEIC"), "x")

	_, err := syncer.Run(context.Background(), mirror.Options{
		SourceDir: src,
		OutputDir: filepath.Join(src, "out"),
	})
	if err == nil {
		t.Fatal("输出目录在源目录内部时应返回错误")
	}
}

// 没有 Exclude 时（全部按未转换处理）所有普通文件都会被复制，路径结构保持不变。
func TestRunCopiesEverythingWithoutExclude(t *testing.T) {
	syncer, _ := newSyncer(t)
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	out := filepath.Join(dir, "output")
	for _, rel := range []string{"2023/12/IMG_1.JPG", "2023/12/IMG_1.MOV", "2024/01/DSC_2.DNG"} {
		writeFile(t, filepath.Join(src, filepath.FromSlash(rel)), rel)
	}

	stats, err := syncer.Run(ctx, mirror.Options{SourceDir: src, OutputDir: out})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Copied != 3 || stats.Failed != 0 {
		t.Fatalf("应复制 3 个文件，实际 %+v", stats)
	}
	for _, rel := range []string{"2023/12/IMG_1.JPG", "2023/12/IMG_1.MOV", "2024/01/DSC_2.DNG"} {
		if body := readFile(t, filepath.Join(out, filepath.FromSlash(rel))); body != rel {
			t.Fatalf("%s 内容不一致", rel)
		}
	}
}
