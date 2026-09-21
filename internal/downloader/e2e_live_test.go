package downloader

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// e2eDouyinLinks 真实回归用例（用户提供的三条抖音图文链接）。
// 覆盖两项修复的线上验证：
//  1. 图片去重：下载后按内容哈希校验无重复图片（修复「框选页相同图片重复出现」）；
//  2. BGM 候选链：music/detail 始终并入链尾后，SSR 地址失效场景下仍能取到 BGM，
//     且 DownloadAudioWithFallback 端到端下载成功。
//
// 默认跳过（需外网 + 消耗平台请求配额）；用 LIVE=1 显式开启：
//
//	LIVE=1 go test ./internal/downloader/ -run TestLiveDouyinE2E -v
func TestLiveDouyinE2E(t *testing.T) {
	if os.Getenv("LIVE") != "1" {
		t.Skip("需要外网，设置 LIVE=1 开启")
	}
	links := []string{
		"https://v.douyin.com/i2rCJHicc/",
		"https://v.douyin.com/i2rLoVMe/",
		"https://v.douyin.com/i2rLoVMe11/",
	}

	// 恢复本机已保存的抖音 Cookie（若存在），贴近真实使用环境
	if v := os.Getenv("LOCALAPPDATA"); v != "" {
		LoadPlatformCookiePersist(filepath.Join(v, "LaMaWatermarkRemover"))
	}

	outRoot := t.TempDir()
	var bgmOK, bgmFail int
	for i, link := range links {
		link := link
		t.Run(fmt.Sprintf("link%d", i+1), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()

			outDir := filepath.Join(outRoot, fmt.Sprintf("post%d", i+1))
			// 与 app 层同路径：短链展开 + 平台分发
			post, err := Download(ctx, link, outDir)
			if err != nil {
				t.Fatalf("下载失败: %v", err)
			}
			t.Logf("图片 %d 张 | 标题 %q | BGM 候选 %d 条", post.Count, post.Title, len(post.AudioCandidates))

			// 校验 1：无重复图片（内容 SHA1 唯一）
			seen := map[string]string{}
			for _, f := range post.Files {
				h, err := fileSHA1ForTest(f)
				if err != nil {
					t.Fatalf("读取 %s: %v", f, err)
				}
				if prev, dup := seen[h]; dup {
					t.Errorf("重复图片: %s 与 %s 内容相同", f, prev)
				}
				seen[h] = f
			}

			// 校验 2：BGM 候选链端到端可下载（无 BGM 的帖子跳过并记录）
			if len(post.AudioCandidates) == 0 {
				t.Log("该帖无 BGM 候选，跳过")
				return
			}
			bgmDir := filepath.Join(outRoot, fmt.Sprintf("bgm%d", i+1))
			saved, err := DownloadAudioWithFallback(ctx, CandidatesFromURLs(post.AudioCandidates), bgmDir, "bgm")
			if err != nil {
				bgmFail++
				t.Errorf("BGM 下载失败（候选 %d 条）: %v", len(post.AudioCandidates), err)
				return
			}
			bgmOK++
			if fi, err := os.Stat(saved); err != nil || fi.Size() < 1024 {
				t.Errorf("BGM 产物异常: %v size=%d", err, fi.Size())
			} else {
				t.Logf("PASS: BGM = %s（%d 字节）", saved, fi.Size())
			}
		})
	}
	t.Logf("汇总：BGM 成功 %d / 失败 %d", bgmOK, bgmFail)
}

// fileSHA1ForTest 计算文件内容 SHA1（与 app.go 的 fileSHA1 语义一致）。
func fileSHA1ForTest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
