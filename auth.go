package main

// 访问门禁：一个进入密码 + 签名 Cookie 会话。
//
// 设计约束（与工程其它部分保持一致）：
//   - 只用标准库，不引第三方依赖，保住 CGO_ENABLED=0 静态编译。
//   - 门禁做在 HTTP 层，不改动 www/index.html：未登录的页面请求重定向到 /login，
//     未登录的 /api/* 请求返回 401，前端拿到 401 走它原有的错误分支。
//   - 密码只在内存里校验；比对用常量时间，避免计时侧信道。

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookie = "qyz_session"
	// 登录态有效期：30 天。单用户自用场景足够，且省去反复输密码。
	sessionTTL = 30 * 24 * time.Hour

	pbkdf2Iterations = 210000
	pbkdf2KeyLen     = 32
	saltLen          = 16

	// 登录失败限流：同一 IP 连续失败 maxLoginFails 次后锁定 lockDuration。
	maxLoginFails = 5
	lockDuration  = 15 * time.Minute
)

// authenticator 持有密码哈希与会话签名密钥，并提供限流状态。
type authenticator struct {
	salted   []byte // PBKDF2 派生出的期望密钥
	salt     []byte
	iter     int
	secret   []byte // 会话 Cookie 的 HMAC 密钥
	mu       sync.Mutex
	failures map[string]*failState
}

type failState struct {
	fails int
	until time.Time
}

// newAuthenticator 由明文密码构造。空密码视为不启用门禁，返回 nil 由上层报错。
func newAuthenticator(password string, secret string) *authenticator {
	if password == "" {
		return nil
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		panic("无法生成随机盐：" + err.Error())
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iterations, pbkdf2KeyLen)
	if err != nil {
		panic("口令派生失败：" + err.Error())
	}

	// 会话密钥优先用显式配置；否则由密码派生，改密码即让旧会话全部失效。
	var sec []byte
	if secret != "" {
		sum := sha256.Sum256([]byte(secret))
		sec = sum[:]
	} else {
		sum := sha256.Sum256(append([]byte("qyz-session|"), []byte(password)...))
		sec = sum[:]
	}

	return &authenticator{
		salted:   key,
		salt:     salt,
		iter:     pbkdf2Iterations,
		secret:   sec,
		failures: make(map[string]*failState),
	}
}

// verifyPassword 常量时间比对派生结果。
func (a *authenticator) verifyPassword(input string) bool {
	key, err := pbkdf2.Key(sha256.New, input, a.salt, a.iter, len(a.salted))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(key, a.salted) == 1
}

func (a *authenticator) sign(payload string) string {
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *authenticator) newSessionToken() string {
	exp := time.Now().Add(sessionTTL).Unix()
	return a.sign(strconv.FormatInt(exp, 10))
}

// sessionValid 校验 Cookie 的签名与有效期。
func (a *authenticator) sessionValid(token string) bool {
	if token == "" {
		return false
	}
	dot := strings.LastIndex(token, ".")
	if dot <= 0 {
		return false
	}
	payload, sig := token[:dot], token[dot+1:]
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(payload))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(sig), []byte(want)) != 1 {
		return false
	}
	exp, err := strconv.ParseInt(payload, 10, 64)
	if err != nil {
		return false
	}
	return time.Now().Unix() < exp
}

/* ===== 限流 ===== */

func (a *authenticator) lockedFor(ip string) time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, ok := a.failures[ip]
	if !ok {
		return 0
	}
	if st.until.After(time.Now()) {
		return time.Until(st.until)
	}
	// 只有"曾经锁定且锁定期已过"才清记录，重新开始计数。
	// 注意：不能在未锁定（until 为零值）时删除——handler 每次登录都会调用本方法，
	// 若在此处误删，失败计数会被不断清零，限流永远无法触发。
	if !st.until.IsZero() {
		delete(a.failures, ip)
	}
	return 0
}

func (a *authenticator) noteFailure(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, ok := a.failures[ip]
	if !ok {
		st = &failState{}
		a.failures[ip] = st
	}
	st.fails++
	if st.fails >= maxLoginFails {
		st.until = time.Now().Add(lockDuration)
		st.fails = 0
	}
	// 简单的内存清理，防止长期运行后 map 无限增长
	if len(a.failures) > 512 {
		for k, v := range a.failures {
			if v.until.Before(time.Now()) && v.fails == 0 {
				delete(a.failures, k)
			}
		}
	}
}

func (a *authenticator) noteSuccess(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.failures, ip)
}

/* ===== 客户端标识 ===== */

// clientIP 取限流用的客户端标识：优先反向代理传来的 X-Forwarded-For 首段，
// 否则用 RemoteAddr。挂在 HTTPS 反代后必须能识别真实来源，否则限流形同虚设。
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if xr := r.Header.Get("X-Real-IP"); xr != "" {
		return strings.TrimSpace(xr)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

/* ===== HTTP 门禁 ===== */

// isLoggedIn 判断请求是否已通过门禁。a 为 nil 表示未启用门禁。
func (a *authenticator) isLoggedIn(r *http.Request) bool {
	if a == nil {
		return true
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	return a.sessionValid(c.Value)
}

// guard 包装下游 handler：未登录时对 /api/* 返回 401 JSON，
// 对页面请求重定向到 /login?next=<原路径>。
func (a *authenticator) guard(next http.Handler) http.Handler {
	if a == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.isLoggedIn(r) {
			next.ServeHTTP(w, r)
			return
		}
		// 放行登录相关路径与健康检查，否则会死循环
		p := r.URL.Path
		if p == "/login" || p == "/api/login" || p == "/api/session" || p == "/api/health" {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(p, "/api/") {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error":  "unauthorized",
				"detail": "请先登录",
			})
			return
		}
		http.Redirect(w, r, "/login?next="+urlQueryEscape(r.URL.RequestURI()), http.StatusFound)
	})
}

// urlQueryEscape 只用于重定向参数转义。
func urlQueryEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9',
			ch == '-', ch == '_', ch == '.', ch == '~':
			b.WriteByte(ch)
		default:
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}

/* ===== 登录页 ===== */

// loginPage 是自包含的登录页：不引外部资源，与项目"离线可用"的取向一致。
func loginPage(next string, message string) string {
	msg := ""
	if message != "" {
		msg = "<p class=\"msg\">" + htmlEscape(message) + "</p>"
	}
	return `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">
<meta name="robots" content="noindex,nofollow">
<title>情谊有账 · 验证</title>
<style>
:root{--paper:#F4EDE1;--surface:#FDFBF6;--ink:#2A211C;--ink2:#7A6A5C;--line:#E4D9C6;--brand:#B23A2E}
*{box-sizing:border-box}
html,body{height:100%}
body{margin:0;background:var(--paper);color:var(--ink);
  font-family:-apple-system,BlinkMacSystemFont,"PingFang SC","Hiragino Sans GB","Microsoft YaHei","Noto Sans CJK SC",sans-serif;
  display:flex;align-items:center;justify-content:center;padding:24px 18px calc(24px + env(safe-area-inset-bottom,0px));
  -webkit-text-size-adjust:100%;-webkit-tap-highlight-color:transparent}
.card{width:100%;max-width:360px;background:var(--surface);border:1px solid var(--line);border-radius:18px;
  padding:28px 22px 24px;box-shadow:0 2px 12px rgba(42,33,28,.06);text-align:center}
.logo{width:54px;height:54px;margin:0 auto 14px;border-radius:50%;background:var(--brand);color:#fff;
  display:flex;align-items:center;justify-content:center;font-size:24px;font-weight:700}
h1{margin:0 0 4px;font-size:19px;letter-spacing:.5px}
.tip{margin:0 0 18px;font-size:13px;color:var(--ink2)}
input{width:100%;min-height:48px;padding:12px;border:1px solid var(--line);border-radius:10px;
  background:#fff;color:var(--ink);font-size:16px;text-align:center;outline:none}
input:focus{border-color:var(--brand);box-shadow:0 0 0 3px rgba(178,58,46,.12)}
button{width:100%;min-height:48px;margin-top:12px;border:0;border-radius:10px;background:var(--brand);
  color:#fff;font-size:16px;font-weight:600;cursor:pointer}
button:disabled{opacity:.55;cursor:not-allowed}
.msg{margin:12px 0 0;font-size:13px;color:#B23A2E}
@media (prefers-color-scheme:dark){
  :root{--paper:#1E1B18;--surface:#262220;--ink:#EDE7DE;--ink2:#A99C8D;--line:#3A3430}
  input{background:#1A1715}
}
</style>
</head>
<body>
<form class="card" method="post" action="/api/login">
  <div class="logo">账</div>
  <h1>情谊有账</h1>
  <p class="tip">请输入访问密码</p>
  <input type="password" name="password" autocomplete="current-password"
         enterkeyhint="go" placeholder="访问密码" autofocus required>
  <input type="hidden" name="next" value="` + htmlEscape(next) + `">
  <button type="submit">进入</button>
  ` + msg + `
</form>
</body>
</html>`
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&#39;")
	return r.Replace(s)
}
