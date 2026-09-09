package downloader

import (
	"crypto/sha1"
	"encoding/hex"
	"html"
	"strings"
)

// htmlUnescape HTML 实体反转义。
func htmlUnescape(s string) string { return html.UnescapeString(s) }

// hashString 短哈希（用作内容 ID 兜底）。
func hashString(s string) string {
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:6])
}

// trimQuery 去掉 URL 查询串。
func trimQuery(u string) string {
	if i := strings.Index(u, "?"); i >= 0 {
		return u[:i]
	}
	return u
}

// reservedWinNames Windows 保留设备名（做文件基名时不可用）。
var reservedWinNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// sanitizeFilename 清洗用户输入的文件名：剔除路径分隔符与 Windows 非法字符、
// 控制字符，trim 首尾空白与点号，超长截断，保留基名避开 Windows 保留名。
// 可能返回空串（调用方需兜底默认名）。
func sanitizeFilename(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			continue // 控制字符
		}
		if strings.ContainsRune(`<>:"/\|?*`, r) {
			continue // 路径分隔与非法字符
		}
		b.WriteRune(r)
	}
	out := strings.Trim(strings.TrimSpace(b.String()), ". ")
	runes := []rune(out)
	if len(runes) > 80 {
		runes = runes[:80]
		out = strings.TrimRight(string(runes), ". ")
	}
	// Windows 保留设备名兜底：con.mp3 等无法创建
	if i := strings.LastIndexByte(out, '.'); i > 0 {
		if reservedWinNames[strings.ToLower(out[:i])] {
			out = "bgm_" + out
		}
	} else if reservedWinNames[strings.ToLower(out)] {
		out = "bgm_" + out
	}
	return out
}
