package main

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
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
