package convert_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/AndreasByrsting/live2motion/internal/convert"
)

// InspectExisting 的三道前置检查都不依赖外部工具，可以在单测里覆盖。
// 真正「合法 Motion Photo 通过校验」的用例需要 exiftool，放在集成测试里。

func TestInspectExistingMissingFile(t *testing.T) {
	p := convert.NewPipeline("heif-convert", "exiftool")
	target := filepath.Join(t.TempDir(), "IMG_0001-motion.jpg")

	_, err := p.InspectExisting(context.Background(), target, 1024)
	if !os.IsNotExist(err) {
		t.Fatalf("不存在时应返回 os.IsNotExist，实际 %v", err)
	}
}

func TestInspectExistingRejectsNonJPEG(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "IMG_0001-motion.jpg")
	// 足够长，不会先被「装不下视频」拦掉，确保走到 JPEG 头判断
	if err := os.WriteFile(target, []byte("definitely not a jpeg, just some bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := convert.NewPipeline("heif-convert", "exiftool")
	if _, err := p.InspectExisting(context.Background(), target, 8); err == nil {
		t.Fatal("非 JPEG 文件应被拒绝")
	}
}

func TestInspectExistingRejectsTruncatedFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "IMG_0001-motion.jpg")
	// 带头部但整体比视频段还短：不可能是完整产物
	data := append([]byte{0xFF, 0xD8}, make([]byte, 64)...)
	if err := os.WriteFile(target, data, 0o644); err != nil {
		t.Fatal(err)
	}

	p := convert.NewPipeline("heif-convert", "exiftool")
	if _, err := p.InspectExisting(context.Background(), target, 4096); err == nil {
		t.Fatal("比视频段还短的文件应被拒绝")
	}
}

func TestOutputRelPathKeepsStructure(t *testing.T) {
	if got := convert.OutputRelPath("-motion", "2024/05/IMG_0001.HEIC"); got != "2024/05/IMG_0001-motion.jpg" {
		t.Fatalf("输出路径应保留源目录结构，得到 %q", got)
	}
	if got := convert.OutputRelPath("", "IMG_0002.HEIC"); got != "IMG_0002.jpg" {
		t.Fatalf("空后缀应直接替换扩展名，得到 %q", got)
	}
}
