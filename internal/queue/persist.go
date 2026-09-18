package queue

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// queueFile queue.json 的文件结构。Version 便于未来格式迁移。
type queueFile struct {
	Version int          `json:"version"` // 1
	Tasks   []*BatchTask `json:"tasks"`
}

// saveMu 跨 Queue 实例串行化磁盘写入（同一进程只应有一个 Queue，防御性兜底）。
var saveMu sync.Mutex

// LoadQueue 从 path 读取任务列表；文件不存在返回空列表（不视为错误）。
func LoadQueue(path string) ([]*BatchTask, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var f queueFile
	if err := json.Unmarshal(data, &f); err != nil {
		// 半写损坏时宁可丢弃历史也不阻断启动（原子写使该分支几乎不可能触发）
		return nil, fmt.Errorf("队列文件解析失败: %w", err)
	}
	return f.Tasks, nil
}

// SaveQueue 原子持久化：先写同目录临时文件再 rename，避免半写损坏
// （PRD §3.3：任务增删与状态迁移时整文件重写）。
func SaveQueue(path string, tasks []*BatchTask) error {
	if path == "" {
		return nil
	}
	saveMu.Lock()
	defer saveMu.Unlock()

	data, err := json.MarshalIndent(queueFile{Version: 1, Tasks: tasks}, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// newID 生成任务 ID：毫秒时间戳 + 8 字节随机数（可读且碰撞概率可忽略）。
func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("t%d_%s", time.Now().UnixMilli(), hex.EncodeToString(b[:]))
}
