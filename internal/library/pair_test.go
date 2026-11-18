package library

import (
	"testing"

	"github.com/immich2motion/immich2motion/internal/store"
)

func heic(id int64, rel, cid string) store.Asset {
	return store.Asset{ID: id, Kind: store.KindHEIC, RelPath: rel, ContentID: cid}
}

func video(id int64, rel, cid string) store.Asset {
	return store.Asset{ID: id, Kind: store.KindVideo, RelPath: rel, ContentID: cid}
}

// 配对唯一依据是 ContentIdentifier，文件名完全不参与判断。
func TestBuildPairsIgnoresFileNames(t *testing.T) {
	assets := []store.Asset{
		heic(1, "2024/05/IMG_1111.HEIC", "UUID-A"),
		video(2, "2024/05/whatever-name.MOV", "UUID-A"),
	}
	pairs, stats := BuildPairs(assets)
	if len(pairs) != 1 {
		t.Fatalf("期望 1 组配对，实际 %d", len(pairs))
	}
	if pairs[0].Heic.ID != 1 || pairs[0].Video.ID != 2 {
		t.Fatalf("配对结果错误: %+v", pairs[0])
	}
	if stats.OrphanHeic != 0 || stats.OrphanVideo != 0 {
		t.Fatalf("不应有孤儿: %+v", stats)
	}
}

// UUID 不一致的 HEIC 与视频即使同目录同文件名也不能配对。
func TestBuildPairsRejectsMismatchedUUID(t *testing.T) {
	assets := []store.Asset{
		heic(1, "a/IMG_0001.HEIC", "UUID-A"),
		video(2, "a/IMG_0001.MOV", "UUID-B"),
	}
	pairs, stats := BuildPairs(assets)
	if len(pairs) != 0 {
		t.Fatalf("UUID 不同不应配对，实际 %d 组", len(pairs))
	}
	if stats.OrphanHeic != 1 || stats.OrphanVideo != 1 {
		t.Fatalf("应各计 1 个孤儿: %+v", stats)
	}
}

// 同目录优先：跨目录的也能配，但同目录的先配。
func TestBuildPairsPrefersSameDirectory(t *testing.T) {
	assets := []store.Asset{
		heic(1, "2024/05/IMG_0001.HEIC", "UUID-A"),
		video(2, "2023/01/IMG_9999.MOV", "UUID-A"),
		video(3, "2024/05/IMG_0001.MOV", "UUID-A"),
	}
	pairs, stats := BuildPairs(assets)
	if len(pairs) != 1 {
		t.Fatalf("期望 1 组配对，实际 %d", len(pairs))
	}
	if pairs[0].Video.ID != 3 {
		t.Fatalf("应优先同目录视频(3)，实际选了 %d", pairs[0].Video.ID)
	}
	if stats.CrossFolder != 0 {
		t.Fatalf("同目录匹配不应计入跨目录: %+v", stats)
	}
}

// 多对多时按路径顺序一对一匹配，多余的全部计入孤儿。
func TestBuildPairsMultipleToOne(t *testing.T) {
	assets := []store.Asset{
		heic(1, "a/IMG_0002.HEIC", "UUID-A"),
		heic(2, "a/IMG_0001.HEIC", "UUID-A"),
		video(3, "a/IMG_0001.MOV", "UUID-A"),
	}
	pairs, stats := BuildPairs(assets)
	if len(pairs) != 1 {
		t.Fatalf("期望 1 组配对，实际 %d", len(pairs))
	}
	// 排序后 IMG_0001.HEIC(2) 优先拿到视频
	if pairs[0].Heic.ID != 2 {
		t.Fatalf("应按路径排序取 IMG_0001.HEIC(2)，实际 %d", pairs[0].Heic.ID)
	}
	if stats.OrphanHeic != 1 {
		t.Fatalf("应剩 1 个孤儿 HEIC: %+v", stats)
	}
}

// 没有 ContentIdentifier 的文件（普通照片）不参与配对，也不计为孤儿。
func TestBuildPairsSkipsAssetsWithoutCID(t *testing.T) {
	assets := []store.Asset{
		heic(1, "a/IMG_0001.HEIC", ""),
		video(2, "a/IMG_0001.MOV", ""),
	}
	pairs, stats := BuildPairs(assets)
	if len(pairs) != 0 {
		t.Fatalf("无 CID 不应配对")
	}
	if stats.OrphanHeic != 0 || stats.OrphanVideo != 0 {
		t.Fatalf("无 CID 不应计为孤儿: %+v", stats)
	}
	if stats.Groups != 0 {
		t.Fatalf("不应产生分组: %+v", stats)
	}
}

func TestBuildPairsDeterministicOrder(t *testing.T) {
	assets := []store.Asset{
		heic(1, "a/x.HEIC", "UUID-Z"),
		video(2, "a/x.MOV", "UUID-Z"),
		heic(3, "a/y.HEIC", "UUID-A"),
		video(4, "a/y.MOV", "UUID-A"),
	}
	pairs, _ := BuildPairs(assets)
	if len(pairs) != 2 {
		t.Fatalf("期望 2 组，实际 %d", len(pairs))
	}
	if pairs[0].ContentID != "UUID-A" || pairs[1].ContentID != "UUID-Z" {
		t.Fatalf("输出应按 CID 排序，实际 %s, %s", pairs[0].ContentID, pairs[1].ContentID)
	}
}

func TestShouldSkipDir(t *testing.T) {
	skip := []string{"@eaDir", "#recycle", ".stversions", ".Trash", "lost+found"}
	for _, name := range skip {
		if !shouldSkipDir(name) {
			t.Errorf("%q 应被跳过", name)
		}
	}
	keep := []string{"2024", "photos", "Immich", "upload"}
	for _, name := range keep {
		if shouldSkipDir(name) {
			t.Errorf("%q 不应被跳过", name)
		}
	}
}

func TestNormalizeCID(t *testing.T) {
	cases := map[string]string{
		"  abc-123  ":   "ABC-123",
		`"ABC-123"`:     "ABC-123",
		"abc\u200b-123": "ABC-123",
		"7B2A1F3C-0000": "7B2A1F3C-0000",
	}
	for in, want := range cases {
		if got := normalizeCID(in); got != want {
			t.Errorf("normalizeCID(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

// exiftool -j -G0 的真实输出格式，验证多组标签的选择优先级。
func TestPickCID(t *testing.T) {
	entry := map[string]any{
		"SourceFile":                  "a.mov",
		"Keys:ContentIdentifier":      "UUID-FROM-KEYS",
		"QuickTime:ContentIdentifier": "UUID-FROM-QT",
	}
	got := pickCID(entry)
	if got.ContentID != "UUID-FROM-KEYS" {
		t.Fatalf("Keys 组优先级更高，实际取到 %q (%s)", got.ContentID, got.Source)
	}

	// 只有 Unknown 组时也要能取到
	entry2 := map[string]any{"Something:ContentIdentifier": "uuid-x"}
	if got := pickCID(entry2); got.ContentID != "UUID-X" {
		t.Fatalf("兜底取值失败: %+v", got)
	}

	// 空值与 Error 字段
	if got := pickCID(map[string]any{"Error": "bad file"}); got.Err != "bad file" {
		t.Fatalf("应返回错误信息: %+v", got)
	}
	if got := pickCID(map[string]any{"SourceFile": "a.heic"}); got.ContentID != "" || got.Err != "" {
		t.Fatalf("无 CID 且无错误时应返回空: %+v", got)
	}
}
