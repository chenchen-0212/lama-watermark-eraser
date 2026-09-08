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
