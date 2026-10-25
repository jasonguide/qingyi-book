package main

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func TestDecodeStateErrorGranularity(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"accounts":[{"id":"a1","name":"x"}]}`, ""},
		{`{"accounts":`, "not valid json"},
		{``, "not valid json"},
		{`{"accounts":[{"id":"a1","name":"x"}],"records":[{"amount":"abc"}]}`, "schema mismatch"},
	}
	for _, c := range cases {
		_, err := decodeState([]byte(c.in))
		got := ""
		var se *schemaError
		if errors.As(err, &se) {
			got = se.msg
		}
		if got != c.want {
			t.Fatalf("decodeState(%q) err=%q want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeConverges(t *testing.T) {
	s := &State{
		Accounts: []Account{{ID: "a1", Name: "  家账  "}, {ID: "a2", Name: ""}},
		Active:   9,
		Records: []Record{
			{ID: "r1", AccountID: "a1", Direction: "weird", Name: " 张三 ", Event: "", Date: "2026-13-01", Amount: -5},
			{ID: "r1", AccountID: "a1", Direction: "out", Name: "李四", Event: "婚礼", Date: "2026-01-02", Amount: 1.005},
			{ID: "<img onerror=x>", AccountID: "a2", Direction: "in", Name: "王五", Event: "满月", Date: "2026-02-03", Amount: 1e12},
			{ID: "r9", AccountID: "ghost", Direction: "in", Name: "孤儿", Amount: 100},
			{ID: "r10", AccountID: "a1", Direction: "in", Name: "   ", Amount: 100},
			{ID: "r11", AccountID: "a1", Direction: "in", Name: "NaN", Amount: math.NaN()},
		},
	}
	if err := normalizeState(s); err != nil {
		t.Fatalf("normalizeState: %v", err)
	}
	if s.Accounts[0].Name != "家账" || s.Accounts[1].Name != "未命名账本" {
		t.Fatalf("account names not cleaned: %+v", s.Accounts)
	}
	if s.Version != 1 || s.Active != 0 {
		t.Fatalf("version/active not fixed: %d %d", s.Version, s.Active)
	}
	if len(s.Records) != 4 {
		t.Fatalf("want 4 surviving records (orphan + blank name dropped), got %d: %+v", len(s.Records), s.Records)
	}
	ids := map[string]bool{}
	for _, r := range s.Records {
		if ids[r.ID] {
			t.Fatalf("duplicate id survived: %s", r.ID)
		}
		ids[r.ID] = true
		if !safeIDPattern.MatchString(r.ID) {
			t.Fatalf("unsafe id survived: %q", r.ID)
		}
		if r.Amount < 0 || r.Amount > maxAmount || math.IsNaN(r.Amount) {
			t.Fatalf("amount not clamped: %v", r.Amount)
		}
		if r.Direction != "in" && r.Direction != "out" {
			t.Fatalf("direction not normalized: %q", r.Direction)
		}
		if r.Date != "" && !isISODate(r.Date) {
			t.Fatalf("bad date survived: %q", r.Date)
		}
		if r.Event == "" {
			t.Fatalf("event not defaulted: %+v", r)
		}
	}
	if r := s.Records[0]; r.Direction != "in" || r.Date != "" || r.Amount != 0 || r.Event != "其他" || r.Name != "张三" {
		t.Fatalf("record 0 normalization wrong: %+v", r)
	}
	if s.Records[1].Amount != 1.01 && s.Records[1].Amount != 1 {
		// math.Round(100.49999)/100 -> 1 ; 只要求落两位有限值
		t.Fatalf("amount round2 unexpected: %v", s.Records[1].Amount)
	}
	if s.Records[2].Amount != maxAmount {
		t.Fatalf("huge amount not clamped: %v", s.Records[2].Amount)
	}
}

func TestNormalizeRejects(t *testing.T) {
	if err := normalizeState(&State{}); err == nil || !strings.Contains(err.Error(), "accounts 不能为空") {
		t.Fatalf("empty accounts not rejected: %v", err)
	}
	big := &State{Accounts: make([]Account, maxAccounts+1)}
	for i := range big.Accounts {
		big.Accounts[i] = Account{ID: "a" + newRecordID()[1:]}
	}
	if err := normalizeState(big); err == nil {
		t.Fatal("over-limit accounts not rejected")
	}
	bad := &State{Accounts: []Account{{ID: "a/../b", Name: "x"}}}
	if err := normalizeState(bad); err == nil || !strings.Contains(err.Error(), "非法字符") {
		t.Fatalf("unsafe account id not rejected: %v", err)
	}
	if err := normalizeState(nil); err == nil {
		t.Fatal("nil state not rejected")
	}
}

func TestIDGenerator(t *testing.T) {
	for i := 0; i < 500; i++ {
		id := newRecordID()
		// "r" + 9 位 base36，与原版重生成的 id 长度一致
		if len(id) != 10 || id[0] != 'r' || !safeIDPattern.MatchString(id) {
			t.Fatalf("bad record id %q", id)
		}
		if a := newAccountID(); len(a) != 10 || a[0] != 'a' || !safeIDPattern.MatchString(a) {
			t.Fatalf("bad account id %q", a)
		}
	}
}
