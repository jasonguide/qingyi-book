package main

// 数据模型与校验/规范化。
//
// 设计要点：前端是原始源码，字段名、字段顺序、JSON 结构都不能改。
// 后端读到的任何数据（磁盘文件、PUT 请求体、导入的备份）都当作不可信输入，
// 统一走 normalizeState 收敛——既做结构校验，也做内容净化，避免脏数据
// 进入渲染层或落盘。

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strings"
	"time"
)

const (
	// 上限与前端 sanitizeState 的 slice 上限保持一致：
	// 超出即拒绝写入，而不是静默截断，避免用户以为数据存下了。
	maxAccounts = 200
	maxRecords  = 200000
	maxAmount   = 1e9 // 单笔金额上限，防止极端值拉爆统计比例尺
)

// 账本 id / 记录 id 会被拼进前端 HTML 属性，必须限制字符集，
// 否则可逃逸属性造成存储型 XSS。规则与前端 SAFE_ID 相同。
var safeIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Account 是一本账本。
type Account struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Record 是一条人情往来。
// 字段顺序刻意与前端输出/原版落盘一致，便于直接比对备份文件。
type Record struct {
	ID        string  `json:"id"`
	AccountID string  `json:"accountId"`
	Direction string  `json:"direction"` // in(收) / out(送)
	Name      string  `json:"name"`
	Event     string  `json:"event"`
	Relation  string  `json:"relation"`
	Date      string  `json:"date"` // YYYY-MM-DD，非法则为空串
	Amount    float64 `json:"amount"`
	Note      string  `json:"note"`
}

// State 是整个账本文件的结构。
type State struct {
	Version  int       `json:"version"`
	Accounts []Account `json:"accounts"`
	Active   int       `json:"active"`
	Records  []Record  `json:"records"`
}

// schemaError 携带面向用户的中文提示。
// 前端会把 detail 直接展示在横幅里，所以文案必须是能看懂的人话。
type schemaError struct{ msg string }

func (e *schemaError) Error() string { return e.msg }

func errSchema(format string, a ...any) error {
	return &schemaError{msg: fmt.Sprintf(format, a...)}
}

// decodeState 解析 JSON 并区分两类失败：
//   - 语法错误 -> "not valid json"
//   - 类型不匹配（例如 amount 传了字符串）-> "schema mismatch"
//
// 这个区分来自实测原版行为，前端也按这两种粒度提示。
func decodeState(data []byte) (*State, error) {
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		var syn *json.SyntaxError
		if errors.As(err, &syn) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, errSchema("not valid json")
		}
		var typ *json.UnmarshalTypeError
		if errors.As(err, &typ) {
			return nil, errSchema("schema mismatch")
		}
		// 空 body 等其它情况统一归为语法问题
		return nil, errSchema("not valid json")
	}
	return &s, nil
}

// normalizeState 校验并就地规范化状态。
// 返回 *schemaError 表示数据不可用；nil 表示已收敛完成。
func normalizeState(s *State) error {
	if s == nil {
		return errSchema("schema mismatch")
	}
	if len(s.Accounts) == 0 {
		return errSchema("accounts 不能为空")
	}
	if len(s.Accounts) > maxAccounts {
		return errSchema("账本数量超出上限")
	}
	if len(s.Records) > maxRecords {
		return errSchema("记录数量超出上限")
	}

	ids := make(map[string]bool, len(s.Accounts))
	for i := range s.Accounts {
		a := &s.Accounts[i]
		if a.ID == "" {
			return errSchema("账本缺少 id")
		}
		if !safeIDPattern.MatchString(a.ID) {
			return errSchema("账本 id 含非法字符")
		}
		a.Name = strings.TrimSpace(a.Name)
		if a.Name == "" {
			a.Name = "未命名账本"
		}
		ids[a.ID] = true
	}

	clean := make([]Record, 0, len(s.Records))
	seen := make(map[string]bool, len(s.Records))
	for _, r := range s.Records {
		// 孤儿记录：所属账本已被删除，直接丢弃，避免出现看不见的数据
		if !ids[r.AccountID] {
			continue
		}
		r.Name = strings.TrimSpace(r.Name)
		if r.Name == "" {
			continue
		}
		// id 非法或重复时重新生成，而不是整份数据判死
		if !safeIDPattern.MatchString(r.ID) || seen[r.ID] {
			r.ID = newRecordID()
		}
		for seen[r.ID] {
			r.ID = newRecordID()
		}
		seen[r.ID] = true

		// 只有明确的 out 才是支出，其余（含历史脏值）一律按收入处理
		if r.Direction != "out" {
			r.Direction = "in"
		}
		r.Event = strings.TrimSpace(r.Event)
		if r.Event == "" {
			r.Event = "其他"
		}
		r.Relation = strings.TrimSpace(r.Relation)
		r.Note = strings.TrimSpace(r.Note)
		if !isISODate(r.Date) {
			r.Date = ""
		}
		r.Amount = round2(clampAmount(r.Amount))

		clean = append(clean, r)
	}
	s.Records = clean

	if s.Version == 0 {
		s.Version = 1
	}
	if s.Active < 0 || s.Active >= len(s.Accounts) {
		s.Active = 0
	}
	return nil
}

// clampAmount 处理 NaN/Inf/负数/超限，保证落盘的一定是有限非负值。
func clampAmount(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	if v > maxAmount {
		return maxAmount
	}
	return v
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// isISODate 只接受严格的 YYYY-MM-DD，且必须是真实存在的日期。
func isISODate(s string) bool {
	if len(s) != 10 {
		return false
	}
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

// newRecordID 生成 "r" + 9 位小写 base36，与原版重生成的 id 长度一致。
func newRecordID() string {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	var b [9]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 极端情况下退化为时间戳，仍满足字符集约束
		return "r" + strings.ToLower(fmt.Sprintf("%09x", time.Now().UnixNano()%0xffffffffff))
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return "r" + string(b[:])
}

// newAccountID 供脚本/测试使用；前端自己会生成 uid。
func newAccountID() string {
	return "a" + strings.TrimPrefix(newRecordID(), "r")
}
