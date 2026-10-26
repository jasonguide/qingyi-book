package main

// 登录 / 登出 / 会话状态 / 健康检查的 HTTP 处理。
//
// 与原版契约的关系：这些是本工程新增的端点，原版没有。
// 之所以新增而不是改前端，是因为 www/index.html 必须与原版保持字节一致。

import (
	"net/http"
	"strings"
	"time"
)

// handleLoginPage 渲染登录页。已登录者直接跳回目标页，避免重复输密码。
func (c config) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if c.auth == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if c.auth.isLoggedIn(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	next := r.URL.Query().Get("next")
	safe := sanitizeNext(next)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(loginPage(safe, "")))
}

// handleLogin 接收表单或 JSON 提交，成功则下发会话 Cookie。
func (c config) handleLogin(w http.ResponseWriter, r *http.Request) {
	if c.auth == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "authDisabled": true})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "请用 POST 提交密码"})
		return
	}

	ip := clientIP(r)
	if d := c.auth.lockedFor(ip); d > 0 {
		msg := "密码错误次数过多，请 " + strconvCeilMinutes(d) + " 分钟后再试"
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"ok": false, "error": "rate limited", "detail": msg, "retryAfter": int(d.Seconds()),
		})
		return
	}

	// 同时兼容表单（登录页）与 JSON（脚本调用）
	password := ""
	next := "/"
	ctype := r.Header.Get("Content-Type")
	if strings.HasPrefix(ctype, "application/json") {
		var body struct {
			Password string `json:"password"`
			Next     string `json:"next"`
		}
		if err := decodeJSONBody(r, &body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "无效 JSON"})
			return
		}
		password = body.Password
		if body.Next != "" {
			next = body.Next
		}
	} else {
		if err := r.ParseForm(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "表单解析失败"})
			return
		}
		password = r.FormValue("password")
		if v := r.FormValue("next"); v != "" {
			next = v
		}
	}

	if !c.auth.verifyPassword(password) {
		c.auth.noteFailure(ip)
		remaining := maxLoginFails - failureCount(c.auth, ip)
		detail := "密码不正确"
		if remaining > 0 && remaining <= 2 {
			detail = "密码不正确，再错 " + itoa(remaining) + " 次将锁定 " + strconvCeilMinutes(lockDuration) + " 分钟"
		}
		if strings.HasPrefix(ctype, "application/json") {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "bad password", "detail": detail})
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(loginPage(sanitizeNext(next), detail)))
		return
	}

	c.auth.noteSuccess(ip)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    c.auth.newSessionToken(),
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// 走 HTTPS 反代时由 QY_COOKIE_SECURE=true 打开；纯内网 http 直连需为 false，
		// 否则浏览器不会回传 Cookie，表现为"登录后又被弹回登录页"。
		Secure: cookieSecure(),
	})

	if strings.HasPrefix(ctype, "application/json") {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	http.Redirect(w, r, sanitizeNext(next), http.StatusFound)
}

// handleLogout 清掉会话。
func (c config) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: cookieSecure(),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSession 让脚本/前端探知当前登录态与门禁是否启用。
func (c config) handleSession(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"authEnabled": c.auth != nil,
		"loggedIn":    c.auth.isLoggedIn(r),
	})
}

// handleHealth 供 Docker HEALTHCHECK 使用，不需要登录。
func (c config) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "time": time.Now().Format(time.RFC3339)})
}

/* ===== 小工具 ===== */

// sanitizeNext 只允许站内相对路径，避免开放重定向。
func sanitizeNext(next string) string {
	if next == "" {
		return "/"
	}
	// 必须单斜杠开头，且不能是 //host 形式（协议相对 URL）
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	if strings.ContainsAny(next, "\r\n") {
		return "/"
	}
	return next
}

// cookieSecure 由环境变量决定，默认 false 以便内网 http 直连即可用。
func cookieSecure() bool {
	v := strings.TrimSpace(strings.ToLower(getenv("QY_COOKIE_SECURE")))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// strconvCeilMinutes 把时长转成"向上取整的分钟数"文案。
func strconvCeilMinutes(d time.Duration) string {
	m := int((d + time.Minute - 1) / time.Minute)
	if m < 1 {
		m = 1
	}
	return itoa(m)
}

func failureCount(a *authenticator, ip string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if st, ok := a.failures[ip]; ok {
		return st.fails
	}
	return 0
}

// decodeJSONBody 限制体积并解析 JSON。
func decodeJSONBody(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 4<<10)
	return jsonNewDecoder(r.Body).Decode(v)
}
