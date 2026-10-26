package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRateLimitLocksAfterFiveFailures(t *testing.T) {
	a := newAuthenticator("secret", "")
	ip := "127.0.0.1"

	if d := a.lockedFor(ip); d != 0 {
		t.Fatalf("初始不应锁定，得到 %v", d)
	}

	for i := 1; i <= maxLoginFails; i++ {
		a.noteFailure(ip)
		// 模拟真实 handler：每次登录前都会查询锁定状态
		d := a.lockedFor(ip)
		a.mu.Lock()
		st := a.failures[ip]
		a.mu.Unlock()
		fails := -1
		if st != nil {
			fails = st.fails
		}
		t.Logf("第 %d 次失败后 lockedFor=%v fails=%d", i, d, fails)
	}

	if d := a.lockedFor(ip); d <= 0 {
		t.Fatalf("连续 %d 次失败后应处于锁定状态，实际 lockedFor=%v", maxLoginFails, d)
	} else {
		t.Logf("锁定生效，剩余 %v", d)
	}
}

func TestGuardBlocksAnon(t *testing.T) {
	a := newAuthenticator("secret", "")
	called := false
	h := a.guard(probeHandler{called: &called})

	// 未登录访问 /api/state -> 401
	req := httptest.NewRequest("GET", "/api/state", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("未登录 /api/state 应 401，实际 %d", rec.Code)
	}

	// 未登录访问页面 -> 302 到 /login
	req2 := httptest.NewRequest("GET", "/", nil)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != 302 {
		t.Fatalf("未登录 / 应 302，实际 %d", rec2.Code)
	}
	if loc := rec2.Header().Get("Location"); loc == "" {
		t.Fatal("302 缺少 Location")
	} else {
		t.Logf("重定向到 %s", loc)
	}
	if called {
		t.Fatal("未登录时下游 handler 不应被调用")
	}
}

func TestSessionTokenTamperRejected(t *testing.T) {
	a := newAuthenticator("secret", "")
	tok := a.newSessionToken()
	if !a.sessionValid(tok) {
		t.Fatal("新签发 token 应有效")
	}
	if a.sessionValid(tok + "x") {
		t.Fatal("篡改后应无效")
	}
	if a.sessionValid("") {
		t.Fatal("空 token 应无效")
	}
	// 另一个密钥签发的 token 不应通过（改密码即失效）
	b := newAuthenticator("other", "")
	if a.sessionValid(b.newSessionToken()) {
		t.Fatal("不同密钥的 token 不应通过")
	}
}

type probeHandler struct {
	called *bool
}

func (p probeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	*p.called = true
	w.WriteHeader(http.StatusOK)
}
