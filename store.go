package main

// 账本文件持久化：读取、原子写入、备份轮转、共享目录探测。
//
// 备份策略与原版一致：保留 3 份 data.json.bak.1..3，.bak.1 最新。
// 每次写入前先轮转，避免用户误操作后无法回溯。

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const maxBackups = 3

// errNoData 表示账本文件尚不存在（首次使用）。
var errNoData = errors.New("no data")

// corruptError 携带面向用户/前端的损坏原因，前端会展示在横幅里。
type corruptError struct{ detail string }

func (e *corruptError) Error() string { return e.detail }

// loadState 读取账本文件并规范化。
//   - 文件不存在 -> errNoData
//   - 读失败/JSON 语法错/结构不合法 -> *corruptError
//
// 注意：这里只在内存里规范化，绝不回写——数据损坏时保持原样，
// 让用户先导出或恢复备份，是原版刻意的保护行为。
func loadState(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errNoData
		}
		return nil, &corruptError{detail: "读取失败"}
	}
	s, err := decodeState(b)
	if err != nil {
		return nil, &corruptError{detail: err.Error()}
	}
	if err := normalizeState(s); err != nil {
		return nil, &corruptError{detail: err.Error()}
	}
	return s, nil
}

// saveState 规范化后写入磁盘。
// 先轮转备份，再写临时文件后 rename，避免写一半断电导致主文件残缺。
func saveState(path string, s *State) error {
	if err := normalizeState(s); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		rotateBackups(path)
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o777); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o666); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// rotateBackups 把当前主文件推进备份链：.2->.3、.1->.2、当前->.1，丢弃最旧的。
func rotateBackups(path string) {
	os.Remove(fmt.Sprintf("%s.bak.%d", path, maxBackups))
	for i := maxBackups - 1; i >= 1; i-- {
		os.Rename(fmt.Sprintf("%s.bak.%d", path, i), fmt.Sprintf("%s.bak.%d", path, i+1))
	}
	if b, err := os.ReadFile(path); err == nil {
		os.WriteFile(path+".bak.1", b, 0o666)
	}
}

type backupInfo struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	ModTime string `json:"modTime"`
}

// listBackups 按 .bak.1..3 顺序列出存在的备份。
func listBackups(path string) []backupInfo {
	out := []backupInfo{}
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	for i := 1; i <= maxBackups; i++ {
		name := fmt.Sprintf("%s.bak.%d", base, i)
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		out = append(out, backupInfo{
			Name:    name,
			Size:    fi.Size(),
			ModTime: fi.ModTime().Format("2006-01-02 15:04:05"),
		})
	}
	return out
}

// splitPaths 解析共享目录配置：兼容冒号分隔（平台下发格式）与逗号分隔（命令行手写）。
func splitPaths(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ':' || r == ',' || r == ';' || r == '\n'
	})
	out := make([]string, 0, len(fields))
	seen := make(map[string]bool, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// pickShareDir 选第一个真实存在的共享目录；没有则返回空串。
func pickShareDir(paths []string) string {
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			return p
		}
	}
	return ""
}
