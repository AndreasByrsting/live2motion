package library

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// MD5File 计算文件 MD5，同时返回读取到的字节数。
//
// 注意：MD5 在这里仅用于完整性校验（检测 bit rot / 输出损坏），
// 不参与配对，也不是安全用途，配对唯一依据是 ContentIdentifier。
func MD5File(path string) (string, int64, error) {
	f, err := os.Open(path) // 只读打开，绝不修改源文件
	if err != nil {
		return "", 0, err
	}
	defer f.Close()

	h := md5.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// VerifyMD5 校验文件 MD5 是否与期望值一致。
func VerifyMD5(path, want string) (bool, error) {
	got, _, err := MD5File(path)
	if err != nil {
		return false, fmt.Errorf("计算 %s 的 MD5 失败: %w", path, err)
	}
	return got == want, nil
}
