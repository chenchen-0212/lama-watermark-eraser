package main

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lama-watermark-eraser/internal/downloader"
	"lama-watermark-eraser/internal/queue"
)

// writeTestPNG 写一张 8x8 纯色 PNG 作为测试夹具。
func writeTestPNG(t *testing.T, path string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 200, B: 200, A: 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("创建 %s: %v", path, err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatalf("编码 PNG %s: %v", path, err)
	}
}

// TestExportSourceZip 源图打包绑定：3 张顶层图入包；子目录干扰项验证
// ziputil.ZipDir 既有递归语义（嵌套文件以相对路径 sub/d.png 入包）；
// zip 命名沿用「源图_<时间戳>.zip」规则（时间戳防重复导出重名）。
func TestExportSourceZip(t *testing.T) {
	src := t.TempDir()
	writeTestPNG(t, filepath.Join(src, "a.png"))
	writeTestPNG(t, filepath.Join(src, "b.png"))
	writeTestPNG(t, filepath.Join(src, "c.png"))
	sub := filepath.Join(src, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestPNG(t, filepath.Join(sub, "d.png"))

	a := &App{} // ExportSourceZip 不依赖 Wails ctx，零值可直接调用
	zipPath, err := a.ExportSourceZip(src)
	if err != nil {
		t.Fatalf("ExportSourceZip: %v", err)
	}
	defer os.Remove(zipPath)

	base := filepath.Base(zipPath)
	if prefix := "源图_"; len(base) < len(prefix) || base[:len(prefix)] != prefix {
		t.Errorf("zip 命名前缀错误: %s（期望前缀 %s）", base, prefix)
	}

	f, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("打开 zip: %v", err)
	}
	defer f.Close()
	got := map[string]bool{}
	for _, zf := range f.File {
		got[zf.Name] = true
	}
	for _, want := range []string{"a.png", "b.png", "c.png", "sub/d.png"} {
		if !got[want] {
			t.Errorf("zip 缺少 %s；实际清单 %v", want, mapKeys(got))
		}
	}
	if len(got) != 4 {
		t.Errorf("zip 条目数错误: %d（期望 4）", len(got))
	}
}

// TestExportSourceZipEmptyDir 空目录/不存在目录应报错而非生成空包。
func TestExportSourceZipEmptyDir(t *testing.T) {
	a := &App{}
	if _, err := a.ExportSourceZip(""); err == nil {
		t.Error("空目录参数应返回错误")
	}
	if _, err := a.ExportSourceZip(filepath.Join(t.TempDir(), "not_exist")); err == nil {
		t.Error("不存在的目录应返回错误")
	}
}

func mapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestDownloadBGMStandaloneEmptyURL 空候选链应在弹对话框之前就报错，
// 保证「该帖子没有 BGM」这条边界不会走到保存流程。
func TestDownloadBGMStandaloneEmptyURL(t *testing.T) {
	a := &App{} // 空候选分支不依赖 Wails ctx
	cases := [][]string{nil, {}, {"", "   "}}
	for _, in := range cases {
		if got, err := a.DownloadBGMStandalone(in, "bgm"); err == nil {
			t.Errorf("candidates=%q 应返回错误，实际得到 %q, nil", in, got)
		}
	}
}

// TestAudioCacheKey 缓存键须忽略签名/时效参数与 CDN 域差异，
// 否则同一曲目每次解析都落到新目录，缓存永不命中。
func TestAudioCacheKey(t *testing.T) {
	a := []string{
		"https://sf3-cdn-tos.douyinstatic.com/obj/ies-music/7286.mp3?x-expires=1&sign=aa",
		"https://sf6-cdn-tos.douyinstatic.com/obj/ies-music/7286.mp3?x-expires=2&sign=bb",
	}
	b := []string{
		"https://sf3-cdn-tos.douyinstatic.com/obj/ies-music/7286.mp3?x-expires=999&sign=zz",
	}
	if audioCacheKey(a) != audioCacheKey(b) {
		t.Errorf("同曲目不同签名应同键: %q vs %q", audioCacheKey(a), audioCacheKey(b))
	}
	// 不同曲目必须不同键
	c := []string{"https://sf3-cdn-tos.douyinstatic.com/obj/ies-music/9999.mp3"}
	if audioCacheKey(b) == audioCacheKey(c) {
		t.Error("不同曲目不应同键")
	}
}

// TestCopyFileStream 单独保存 BGM 用的流式复制：内容一致、目标扩展名补全由调用方负责。
func TestCopyFileStream(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "bgm.mp3")
	payload := []byte("ID3\x03\x00 fake-audio-payload")
	if err := os.WriteFile(src, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "out", "saved.mp3")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFileStream(src, dst); err != nil {
		t.Fatalf("copyFileStream: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Errorf("复制内容不一致: %q", got)
	}

	if err := copyFileStream(filepath.Join(dir, "not_exist.mp3"), dst); err == nil {
		t.Error("源文件不存在时应返回错误")
	}
	if err := copyFileStream(src, filepath.Join(dir, "no_such_dir", "x.mp3")); err == nil {
		t.Error("目标目录不存在时应返回错误")
	}
}

// TestGetBGMAudioLocalFile BGM 试听：已下载的本地文件应被直接读取为带正确 MIME 的
// data URL，且优先于 audioURL（远端地址非法也不应触发网络请求）。
func TestGetBGMAudioLocalFile(t *testing.T) {
	dir := t.TempDir()
	payload := []byte("M4A\x00fake-audio-payload-for-preview")
	cases := []struct {
		file   string
		prefix string
	}{
		{"bgm.m4a", "data:audio/mp4;base64,"},
		{"music.mp3", "data:audio/mpeg;base64,"},
		{"odd.bin", "data:audio/mpeg;base64,"}, // 未知扩展名按 mp3 兜底
	}
	a := &App{}
	for _, c := range cases {
		path := filepath.Join(dir, c.file)
		if err := os.WriteFile(path, payload, 0o644); err != nil {
			t.Fatal(err)
		}
		// candidates 传一个必然连不通的地址：本地文件命中时不应走下载分支
		got, err := a.GetBGMAudio([]string{"http://127.0.0.1:1/should-not-be-fetched.mp3"}, path)
		if err != nil {
			t.Fatalf("GetBGMAudio(%s): %v", c.file, err)
		}
		if !strings.HasPrefix(got, c.prefix) {
			t.Errorf("%s 前缀错误，期望 %s，实际 %.40s", c.file, c.prefix, got)
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(got, c.prefix))
		if err != nil {
			t.Fatalf("%s base64 解码失败: %v", c.file, err)
		}
		if !bytes.Equal(raw, payload) {
			t.Errorf("%s 内容不一致: %q", c.file, raw)
		}
	}
}

// TestGetBGMAudioNoSource 无本地文件且无音频地址（或地址不可用）时应报错而非 panic。
func TestGetBGMAudioNoSource(t *testing.T) {
	a := &App{}
	for _, in := range [][]string{nil, {}, {"", "   "}} {
		got, err := a.GetBGMAudio(in, "")
		if err == nil {
			t.Errorf("candidates=%q 应返回错误，实际得到 %.40q", in, got)
			continue
		}
		// 空候选要落到面向用户的「无 BGM」提示，而不是底层地址错误。
		// GetBGMAudio 与 downloader 层的文案不同，两者都算友好提示。
		msg := err.Error()
		if !strings.Contains(msg, "未解析到 BGM 音频地址") && !strings.Contains(msg, "没有可") {
			t.Errorf("candidates=%q 错误信息不友好: %v", in, err)
		}
	}
	// localPath 指向不存在的文件时，退回到音频地址判断（同样报错）
	if _, err := a.GetBGMAudio(nil, filepath.Join(t.TempDir(), "nope.mp3")); err == nil {
		t.Error("不存在的本地文件 + 空音频地址应返回错误")
	}
}

// TestGetBGMAudioDownloadAndCache 未下载时从音频地址抓到本机试听缓存；
// 同一地址二次调用命中缓存，不再发起 HTTP 请求。
func TestGetBGMAudioDownloadAndCache(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir()) // 隔离试听缓存目录，避免污染真实应用数据

	var hits int32
	payload := []byte("ID3\x03\x00fake-mp3-bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	url := srv.URL + "/music/abc.mp3"
	a := &App{}

	got, err := a.GetBGMAudio([]string{url}, "")
	if err != nil {
		t.Fatalf("GetBGMAudio: %v", err)
	}
	const prefix = "data:audio/mpeg;base64,"
	if !strings.HasPrefix(got, prefix) {
		t.Fatalf("前缀错误，期望 %s，实际 %.40s", prefix, got)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(got, prefix))
	if err != nil {
		t.Fatalf("base64 解码失败: %v", err)
	}
	if !bytes.Equal(raw, payload) {
		t.Errorf("音频内容不一致: %q", raw)
	}

	if _, err := a.GetBGMAudio([]string{url}, ""); err != nil {
		t.Fatalf("第二次 GetBGMAudio: %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("缓存未生效：期望只请求 1 次，实际 %d 次", n)
	}
}

// TestGetBGMAudioFallback 首个候选为风控页（非音频）时，应自动落到第二个候选。
func TestGetBGMAudioFallback(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>风控拦截页</html>"))
	}))
	defer bad.Close()

	payload := []byte("ID3\x03\x00real-audio-bytes")
	var goodHits int32
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&goodHits, 1)
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(payload)
	}))
	defer good.Close()

	a := &App{}
	got, err := a.GetBGMAudio([]string{bad.URL + "/a.mp3", good.URL + "/b.mp3"}, "")
	if err != nil {
		t.Fatalf("候选链应落到第二个地址，实际报错: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(got, "data:audio/mpeg;base64,"))
	if err != nil {
		t.Fatalf("base64 解码失败: %v", err)
	}
	if !bytes.Equal(raw, payload) {
		t.Errorf("音频内容不一致: %q", raw)
	}
	if n := atomic.LoadInt32(&goodHits); n != 1 {
		t.Errorf("第二候选应被请求 1 次，实际 %d 次", n)
	}
}

// TestGetBGMAudioRejectsHTML 全部候选都不是音频时必须报错，绝不把风控页当音频返回。
func TestGetBGMAudioRejectsHTML(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body>访问过于频繁</body></html>"))
	}))
	defer srv.Close()

	a := &App{}
	got, err := a.GetBGMAudio([]string{srv.URL + "/blocked.mp3"}, "")
	if err == nil {
		t.Fatalf("风控页不应被当作音频返回，实际得到 %.60q", got)
	}
	if !strings.Contains(err.Error(), "不是音频") {
		t.Errorf("错误信息应指明内容不是音频，实际: %v", err)
	}
}

// TestBGMAudioCacheHitOnCDNDrift CDN 每次下发的地址都带新的签名/时效参数，
// 缓存键取候选链的路径签名（而非完整 URL），因此地址漂移后仍应命中缓存。
func TestBGMAudioCacheHitOnCDNDrift(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())

	var hits int32
	payload := []byte("ID3\x03\x00cached-audio-bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	a := &App{}
	base := srv.URL + "/obj/ies-music/7286.mp3"
	if _, err := a.GetBGMAudio([]string{base + "?x-expires=1&sign=aa"}, ""); err != nil {
		t.Fatalf("首次取音频: %v", err)
	}
	if _, err := a.GetBGMAudio([]string{base + "?x-expires=999&sign=zz"}, ""); err != nil {
		t.Fatalf("二次取音频: %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("签名漂移后应命中缓存（仅 1 次请求），实际 %d 次", n)
	}
}

// TestBGMAudioCacheKeyUsesMusicID 登记了 music_id 的候选链，即使各级候选的文件
// 路径完全不同（SSR 与 detail 接口常下发不同的对象存储路径），也应落到同一个
// 试听缓存——同一首 BGM 不该被重复下载。
func TestBGMAudioCacheKeyUsesMusicID(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())

	var hits int32
	payload := []byte("ID3\x03\x00music-id-cache-bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	const musicID = "7286820160101201977"
	ssr := []string{srv.URL + "/obj/ies-music/7286.mp3?sign=aa"}
	detail := []string{srv.URL + "/obj/tos-cn-ve-2774/other-file?sign=bb"}
	downloader.RegisterAudioChain(ssr, []downloader.AudioCandidate{
		{URL: ssr[0], Source: downloader.SourceDouyinSSRURLList},
	}, musicID)
	downloader.RegisterAudioChain(detail, []downloader.AudioCandidate{
		{URL: detail[0], Source: downloader.SourceDouyinMusicDetail},
	}, musicID)

	a := &App{}
	if _, err := a.GetBGMAudio(ssr, ""); err != nil {
		t.Fatalf("首次取音频: %v", err)
	}
	if _, err := a.GetBGMAudio(detail, ""); err != nil {
		t.Fatalf("二次取音频: %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("同一 music_id 的不同候选链应共用缓存（仅 1 次请求），实际 %d 次", n)
	}
}

// TestGetAppVersion 版本号注入行为：
//   - 未注入（go test / go run 场景）必须回退为 "dev"，不能返回空串——
//     前端据此决定是否显示版本标注，空串与「未注入」在语义上要区分开；
//   - 注入后原样返回，保证与安装包版本同源。
//
// 测试直接改包级变量并 defer 还原：该变量只在 main 包内由 ldflags 写入，
// 没有并发写风险。
func TestGetAppVersion(t *testing.T) {
	orig := appVersion
	defer func() { appVersion = orig }()

	appVersion = ""
	if got := (&App{}).GetAppVersion(); got != "dev" {
		t.Errorf("未注入时应回退为 dev，实际 %q", got)
	}

	appVersion = "1.1.6"
	if got := (&App{}).GetAppVersion(); got != "1.1.6" {
		t.Errorf("注入后应原样返回，实际 %q", got)
	}
}

// TestInstallDirAndQueuePath 安装目录与任务数据路径位于 <安装目录>/dataList。
func TestInstallDirAndQueuePath(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("取不到测试可执行文件路径: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if got, want := installDir(), filepath.Dir(exe); got != want {
		t.Errorf("installDir 应为 exe 所在目录\n got=%s\nwant=%s", got, want)
	}
	if got, want := queueFilePath(), filepath.Join(filepath.Dir(exe), "dataList", "queue.json"); got != want {
		t.Errorf("任务数据路径错误\n got=%s\nwant=%s", got, want)
	}
}

// TestMigrateLegacyQueue 旧位置的任务数据应被一次性迁移到新位置；
// 新位置已有数据时不覆盖，避免把用户当前任务冲掉。
func TestMigrateLegacyQueue(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "queue.json")

	// 旧文件存在 + 新文件不存在 → 迁移并删除旧文件
	old := filepath.Join(src, "queue.json")
	content := []byte(`{"version":1,"tasks":[{"id":"legacy-1","type":"download","state":"done"}]}`)
	if err := os.WriteFile(old, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := migrateLegacyQueueFrom(old, dst); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("迁移后新位置应有数据: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("迁移内容不一致\n got=%s\nwant=%s", got, content)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("迁移成功后旧文件应被删除（否则新位置清空后旧任务会复活）")
	}

	// 新文件已存在 → 不迁移、不覆盖
	fresh := []byte(`{"version":1,"tasks":[{"id":"cur-1"}]}`)
	if err := os.WriteFile(dst, fresh, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := migrateLegacyQueueFrom(old, dst); err != nil {
		t.Fatalf("已有数据时迁移应静默跳过: %v", err)
	}
	got, _ = os.ReadFile(dst)
	if string(got) != string(fresh) {
		t.Errorf("新位置已有数据时不应被覆盖\n got=%s", got)
	}
	if _, err := os.Stat(old); err != nil {
		t.Error("跳过迁移时旧文件应保持原样")
	}

	// 旧文件不存在 → 全新安装，静默返回
	if err := migrateLegacyQueueFrom(filepath.Join(src, "nope.json"), dst); err != nil {
		t.Errorf("旧文件缺失时应静默返回，实际 %v", err)
	}
}

// ---------------------------------------------------------- 缓存清理

// mkFiles 在目录下批量创建指定大小的文件，返回总字节数与文件数。
func mkFiles(t *testing.T, dir string, sizes ...int) (int64, int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var total int64
	for i, n := range sizes {
		p := filepath.Join(dir, fmt.Sprintf("f%d.bin", i))
		if err := os.WriteFile(p, bytes.Repeat([]byte("x"), n), 0o644); err != nil {
			t.Fatal(err)
		}
		total += int64(n)
	}
	return total, len(sizes)
}

// TestIsReferenced 引用判定必须同时覆盖「目录包含文件」与「文件反查目录」
// 两个方向——漏掉任何一侧都会误删仍在使用中的素材。
func TestIsReferenced(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "downloads", "douyin", "abc")
	file := filepath.Join(dir, "01.webp")
	other := filepath.Join(root, "downloads", "douyin", "zzz", "01.webp")

	used := []string{dir} // 任务记录里存的是目录
	if !isReferenced(used, dir) {
		t.Error("目录自身应视为被引用")
	}
	if !isReferenced(used, file) {
		t.Error("目录内的文件应视为被引用（否则会被整目录清掉）")
	}
	if isReferenced(used, other) {
		t.Error("兄弟目录不应被视为被引用")
	}

	// 反向：记录里存的是文件，其所在目录也不该被整目录删除
	usedFile := []string{file}
	if !isReferenced(usedFile, dir) {
		t.Error("被引用文件所在目录不应被视为可释放")
	}

	// 大小写不敏感（Windows 语义）：盘符/目录名大小写不同不能造成误判
	upper := strings.ToUpper(filepath.ToSlash(dir))
	if !isReferenced(used, filepath.FromSlash(upper)) {
		t.Error("路径比较应忽略大小写，避免大小写差异导致误删")
	}
}

// TestClearWorkspaceCacheKeepsReferenced 核心安全约束：被任务记录引用的
// 目录与文件必须原样保留，只有「无记录指向」的旧素材才被清掉。
func TestClearWorkspaceCacheKeepsReferenced(t *testing.T) {
	root := t.TempDir()
	keptDir := filepath.Join(root, "downloads", "douyin", "keep-me")
	keptSize, _ := mkFiles(t, keptDir, 100, 200)
	junkDir := filepath.Join(root, "downloads", "douyin", "old-junk")
	junkSize, _ := mkFiles(t, junkDir, 300, 400)
	// 顶层散落的旧 zip（导出遗留）
	zipPath := filepath.Join(root, "去水印结果_20260901_000000.zip")
	if err := os.WriteFile(zipPath, bytes.Repeat([]byte("z"), 500), 0o644); err != nil {
		t.Fatal(err)
	}

	a := newCacheTestApp([]string{keptDir})
	payload, err := a.clearWorkspaceCacheAt(root)
	if err != nil {
		t.Fatalf("清除失败: %v", err)
	}

	if _, err := os.Stat(keptDir); err != nil {
		t.Fatalf("被任务记录引用的目录必须保留: %v", err)
	}
	if _, err := os.Stat(filepath.Join(keptDir, "f0.bin")); err != nil {
		t.Error("被引用目录内的文件必须保留")
	}
	if _, err := os.Stat(junkDir); !os.IsNotExist(err) {
		t.Error("未被引用的目录应被删除")
	}
	if _, err := os.Stat(zipPath); !os.IsNotExist(err) {
		t.Error("未被引用的顶层 zip 应被删除")
	}

	wantFreed := junkSize + 500
	if payload.FreedBytes != int64(wantFreed) {
		t.Errorf("释放字节数不符\n got=%d\nwant=%d", payload.FreedBytes, wantFreed)
	}
	if payload.Kept <= 0 {
		t.Error("应报告有受保护的条目")
	}
	if payload.Nothing {
		t.Error("确实清掉了文件，不应标记 Nothing")
	}
	_ = keptSize
}

// TestClearWorkspaceCachePartialDir 目录被部分引用时，只清内部未被引用的子项，
// 目录自身与已引用文件保留。
func TestClearWorkspaceCachePartialDir(t *testing.T) {
	root := t.TempDir()
	plat := filepath.Join(root, "downloads", "douyin")
	keep := filepath.Join(plat, "keep")
	drop := filepath.Join(plat, "drop")
	mkFiles(t, keep, 111)
	mkFiles(t, drop, 222)

	a := newCacheTestApp([]string{keep})
	if _, err := a.clearWorkspaceCacheAt(root); err != nil {
		t.Fatalf("清除失败: %v", err)
	}

	if _, err := os.Stat(keep); err != nil {
		t.Error("被引用的子目录应保留")
	}
	if _, err := os.Stat(drop); !os.IsNotExist(err) {
		t.Error("同层未被引用的子目录应被删除")
	}
	// 容器目录 download/douyin 本身未被任何记录引用，但其内部仍有
	// 被引用的 keep/，故不能让「删掉容器」波及 keep——要么容器保留，
	// 要么容器被删但 keep 必须还在（keep 已在上面断言）。
	_ = plat
}

// TestClearWorkspaceCacheNothing 无可释放空间时应返回 Nothing，且不报错——
// 前端据此提示「已是干净状态」而不是「已清除 0 B」。
func TestClearWorkspaceCacheNothing(t *testing.T) {
	root := t.TempDir()
	keep := filepath.Join(root, "downloads", "xhs", "only")
	mkFiles(t, keep, 64)

	a := newCacheTestApp([]string{keep})
	payload, err := a.clearWorkspaceCacheAt(root)
	if err != nil {
		t.Fatalf("无可清理内容时不应报错: %v", err)
	}
	if !payload.Nothing {
		t.Error("无可释放空间时应标记 Nothing")
	}
	if payload.FreedBytes != 0 {
		t.Errorf("不应报告释放空间，实际 %d", payload.FreedBytes)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("唯一目录被引用，应原样保留")
	}
}

// TestClearWorkspaceCacheMissingDir 目录不存在时静默返回 Nothing，不报错。
func TestClearWorkspaceCacheMissingDir(t *testing.T) {
	a := newCacheTestApp(nil)
	payload, err := a.clearWorkspaceCacheAt(filepath.Join(t.TempDir(), "not-created"))
	if err != nil {
		t.Fatalf("目录不存在不应报错: %v", err)
	}
	if !payload.Nothing {
		t.Error("目录不存在应标记 Nothing")
	}
}

// TestGetCacheUsageAt 用量统计：可释放 / 受保护两侧都要算准。
func TestGetCacheUsageAt(t *testing.T) {
	root := t.TempDir()
	keep := filepath.Join(root, "downloads", "wechat", "k")
	keepTotal, keepN := mkFiles(t, keep, 1000, 1000)
	junk := filepath.Join(root, "selected", "20260901_000000")
	junkTotal, junkN := mkFiles(t, junk, 250, 250)

	a := newCacheTestApp([]string{keep})
	u := a.getCacheUsageAt(root)

	if u.TotalBytes != keepTotal+junkTotal {
		t.Errorf("总占用不符\n got=%d\nwant=%d", u.TotalBytes, keepTotal+junkTotal)
	}
	if u.FreeBytes != junkTotal {
		t.Errorf("可释放不符\n got=%d\nwant=%d", u.FreeBytes, junkTotal)
	}
	if u.KeptBytes != keepTotal {
		t.Errorf("受保护不符\n got=%d\nwant=%d", u.KeptBytes, keepTotal)
	}
	if u.KeptDir != 1 {
		t.Errorf("受保护目录数应为 1，实际 %d", u.KeptDir)
	}
	_ = keepN
	_ = junkN
}

// TestRemovePathsUnderStaysInsideRoot 边界约束：只删 root 之内、且不允许删 root 本身。
// 记录可能被手改指向工作区外的目录，越界删除会误伤用户文件。
func TestRemovePathsUnderStaysInsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	// 工作区内
	inside := filepath.Join(root, "downloads", "a")
	mkFiles(t, inside, 128)
	// 工作区外（同级的另一个目录，前缀相同但不是子路径）
	outFile := filepath.Join(outside, "keep.bin")
	if err := os.WriteFile(outFile, []byte("important"), 0o644); err != nil {
		t.Fatal(err)
	}

	freed, n := removePathsUnder(root, []string{inside, outFile, root})
	if freed <= 0 || n <= 0 {
		t.Errorf("应删除工作区内的目录，实际 freed=%d n=%d", freed, n)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("工作区外的目录绝不能被删除: %v", err)
	}
	if _, err := os.Stat(outFile); err != nil {
		t.Error("工作区外的文件绝不能被删除")
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatal("root 自身绝不能被删除")
	}
}

// newCacheTestApp 构造仅带「被引用路径」的 App，供缓存清理用例使用。
func newCacheTestApp(referenced []string) *App {
	if len(referenced) == 0 {
		return &App{}
	}
	now := time.Now()
	tasks := make([]queue.BatchTask, 0, len(referenced))
	for i, p := range referenced {
		tasks = append(tasks, queue.BatchTask{
			ID:        fmt.Sprintf("t-%d", i),
			Type:      queue.TypeDownload,
			State:     queue.StateDone,
			ResultDir: p,
			FinishedAt: &now,
		})
	}
	return &App{q: queue.NewWithTasks(tasks, "")}
}

// taskIDs 提取任务 ID 列表（测试断言用）。
func taskIDs(ts []queue.BatchTask) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.ID)
	}
	return out
}
