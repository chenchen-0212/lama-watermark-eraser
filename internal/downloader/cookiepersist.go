package downloader

import (
	"os"
	"path/filepath"
	"strings"
)

// SetPlatformCookiePersist 设置平台 Cookie 并持久化到 dir/{platform}_cookie.txt。
// raw 为空白时视为清除：删除内存中的 Cookie 与持久化文件。
func SetPlatformCookiePersist(dir, platform, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		SetPlatformCookie(platform, "")
		file := filepath.Join(dir, platform+"_cookie.txt")
		if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	SetPlatformCookie(platform, raw)
	return os.WriteFile(filepath.Join(dir, platform+"_cookie.txt"), []byte(raw), 0o600)
}

// LoadPlatformCookiePersist 从 dir 加载所有 {platform}_cookie.txt 到内存，
// 供启动时恢复。目录不存在或读取失败时静默跳过（Cookie 属可选配置）。
func LoadPlatformCookiePersist(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, "_cookie.txt") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if raw := strings.TrimSpace(string(data)); raw != "" {
			SetPlatformCookie(strings.TrimSuffix(name, "_cookie.txt"), raw)
		}
	}
}
