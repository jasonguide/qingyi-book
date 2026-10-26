package main

// HTTP 层：配置、监听、路由、静态资源。
//
// 运行模型由环境变量决定：
//   SOCKET                网关模式下的 Unix Socket 路径；为空则退回 TCP 端口模式
//   PORT                  TCP 端口，默认 6189
//   GATEWAY_PREFIX        网关挂载前缀（如 /app/qingyi-book），分发前剥掉
//   WWW                   静态资源目录
//   DATA_FILE             账本 JSON 落盘路径
//   TRIM_DATA_SHARE_PATHS 共享目录列表，导出到 NAS 时取第一个真实存在的
//
// 响应体一律用 map 而不是 struct：Go 序列化 map 时按 key 排序，
// 与原版响应字节序一致（前端不依赖顺序，但比对时能一眼看出差异）。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const (
	defPort   = "6189"
	maxUpload = 1 << 20  // 导入上限 1MB：实测原版超过即报"无法解析"
	maxBody   = 64 << 20 // 保存上限：记录数量上限决定实际触不到
)

type config struct {
	socket   string
	prefix   string
	www      string
	dataFile string
	shares   []string
	port     string
	// auth 为 nil 表示未启用门禁；由 QY_PASSWORD 构造。
	auth *authenticator
}

func loadConfig() config {
	c := config{
		socket:   os.Getenv("SOCKET"),
		prefix:   strings.TrimSuffix(os.Getenv("GATEWAY_PREFIX"), "/"),
		www:      os.Getenv("WWW"),
		dataFile: os.Getenv("DATA_FILE"),
		port:     os.Getenv("PORT"),
		shares:   splitPaths(os.Getenv("TRIM_DATA_SHARE_PATHS")),
	}
	if c.port == "" {
		c.port = defPort
	}
	if c.www == "" {
		c.www = "www"
	}
	if c.dataFile == "" {
		c.dataFile = "data.json"
	}
	c.auth = newAuthenticator(os.Getenv("QY_PASSWORD"), os.Getenv("QY_SESSION_SECRET"))
	return c
}

func main() {
	cfg := loadConfig()

	var ln net.Listener
	var err error
	if cfg.socket != "" {
		ln, err = listenUnix(cfg.socket)
	} else {
		ln, err = net.Listen("tcp", ":"+cfg.port)
	}
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("qingyi-book listening %s:%s  web=%s  data=%s  shares=%s",
		ln.Addr().Network(), ln.Addr().String(), cfg.www, cfg.dataFile, strings.Join(cfg.shares, ":"))
	if cfg.auth == nil {
		log.Printf("警告：未设置 QY_PASSWORD，访问门禁未启用，任何能访问该端口的人都能查看和修改账本")
	} else {
		log.Printf("访问门禁已启用（会话 %s，登录失败 %d 次锁定 %s）", sessionTTL, maxLoginFails, lockDuration)
	}

	srv := &http.Server{Handler: cfg.routes(), ReadHeaderTimeout: 15 * time.Second}

	// 平台 stop 发 TERM，给在途请求 5s 收尾；脚本等 10s 后才 KILL。
	done := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		close(done)
	}()

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("serve: %v", err)
	}
	<-done
}

// listenUnix 绑定 Socket。上次异常退出可能留下失效文件，探测后清理重试。
func listenUnix(path string) (net.Listener, error) {
	ln, err := net.Listen("unix", path)
	if err == nil {
		return ln, nil
	}
	if rmErr := os.Remove(path); rmErr != nil {
		return nil, err
	}
	return net.Listen("unix", path)
}

func (c config) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/login", c.handleLoginPage)
	mux.HandleFunc("/api/login", c.handleLogin)
	mux.HandleFunc("/api/logout", c.handleLogout)
	mux.HandleFunc("/api/session", c.handleSession)
	mux.HandleFunc("/api/health", c.handleHealth)
	mux.HandleFunc("/api/state", c.handleState)
	mux.HandleFunc("/api/export/xlsx", c.handleExportXlsx)
	mux.HandleFunc("/api/export/to-nas", c.handleExportToNas)
	mux.HandleFunc("/api/import/xlsx", c.handleImportXlsx)
	mux.HandleFunc("/api/backups", c.handleBackups)
	mux.HandleFunc("/api/storage", c.handleStorage)
	mux.Handle("/", http.FileServer(http.Dir(c.www)))

	// 门禁包在最外层：网关前缀剥离后仍能正确判定登录态。
	guarded := c.auth.guard(mux)

	if c.prefix == "" {
		return guarded
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == c.prefix {
			// 网关有时不带尾斜杠，直接当首页，省一跳
			r.URL.Path = "/"
		} else if strings.HasPrefix(p, c.prefix+"/") {
			r.URL.Path = strings.TrimPrefix(p, c.prefix)
		}
		guarded.ServeHTTP(w, r)
	})
}

/* ===== 账本读写 ===== */

func (c config) handleState(w http.ResponseWriter, r *http.Request) {
	// 原版只区分 GET 与"其它"：非 GET 一律按保存处理
	if r.Method == http.MethodGet {
		c.getState(w)
		return
	}
	c.putState(w, r)
}

// getState 只校验、不改写：脏但结构合法的数据原样交给前端，
// 由前端 sanitizeState 收敛。数据损坏时明确报 500，绝不静默当空账本。
func (c config) getState(w http.ResponseWriter) {
	raw, err := os.ReadFile(c.dataFile)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "no data"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"detail": "读取失败",
			"error":  "data corrupted",
		})
		return
	}
	s, err := decodeState(raw)
	if err == nil {
		err = normalizeState(s)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"detail": err.Error(),
			"error":  "data corrupted",
		})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(raw)
}

func (c config) putState(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		badState(w, "not valid json")
		return
	}
	s, err := decodeState(raw)
	if err == nil {
		err = normalizeState(s)
	}
	if err != nil {
		badState(w, err.Error())
		return
	}
	if err := saveState(c.dataFile, s); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "写入失败：" + err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func badState(w http.ResponseWriter, detail string) {
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"detail": detail,
		"error":  "invalid state schema",
	})
}

/* ===== 导出 ===== */

// handleExportXlsx 不校验方法：原版对任何方法都返回文件内容。
func (c config) handleExportXlsx(w http.ResponseWriter, r *http.Request) {
	s, err := loadState(c.dataFile)
	if err != nil {
		if errors.Is(err, errNoData) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "no data"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "data corrupted"})
		return
	}
	data, err := buildXLSX(s)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "data corrupted"})
		return
	}
	w.Header().Set("Content-Type", xlsxContentType)
	_, _ = w.Write(data)
}

// handleExportToNas 由服务端直接落盘到共享目录，不经浏览器下载。
func (c config) handleExportToNas(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s, err := loadState(c.dataFile)
	if err != nil {
		if errors.Is(err, errNoData) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "暂无可导出的数据", "ok": false})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "账本数据已损坏，请先恢复备份", "ok": false})
		return
	}
	dir := pickShareDir(c.shares)
	if dir == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "未检测到共享目录，无法导出到 NAS",
			"ok":    false,
		})
		return
	}
	data, err := buildXLSX(s)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "生成失败：" + err.Error(),
			"ok":    false,
		})
		return
	}
	name := "情谊有账-" + time.Now().Format("20060102-150405") + ".xlsx"
	path := dir + "/" + name
	if err := os.WriteFile(path, data, 0o644); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "导出失败：" + err.Error(),
			"ok":    false,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "ok": true, "path": path})
}

/* ===== 导入 ===== */

func (c config) handleImportXlsx(w http.ResponseWriter, r *http.Request) {
	// 不校验请求方法：原版对 GET/PUT 同样尝试解析 multipart
	// 1MB 上限与原版一致：超限时 multipart 读取失败，统一报格式问题
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	if err := r.ParseMultipartForm(maxUpload); err != nil {
		importFail(w, "文件过大或格式不正确")
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		importFail(w, "未收到文件")
		return
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		importFail(w, "文件过大或格式不正确")
		return
	}
	rows, skipped, truncated, err := parseXLSX(data)
	if err != nil {
		// 原版此处返回 200 + ok:false，前端按 toast/横幅提示
		writeJSON(w, http.StatusOK, map[string]any{
			"error": "无法解析该 Excel 文件，请使用导出的 .xlsx",
			"ok":    false,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"rows":      rows,
		"skipped":   skipped,
		"truncated": truncated,
	})
}

/* ===== 备份与存储探测 ===== */

func (c config) handleBackups(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"backups": listBackups(c.dataFile)})
}

func (c config) handleStorage(w http.ResponseWriter, r *http.Request) {
	// shares 为空时序列化成 null，与原版一致
	writeJSON(w, http.StatusOK, map[string]any{
		"dataFile":   c.dataFile,
		"sharePaths": c.shares,
	})
}

// importFail 是导入失败的统一出口：一律 400 + ok:false。
func importFail(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": msg, "ok": false})
}

/* ===== 响应工具 ===== */

func writeJSON(w http.ResponseWriter, code int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(data)
}
