// Package core 是 FileDrop 的服务端核心：HTTP 路由 + 分块上传落盘 + 下载 + 文件管理。
// 前端静态资源通过 go:embed 编进二进制，因此成品是真正的单文件可执行程序。
package core

import (
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/skip2/go-qrcode"
)

//go:embed all:web
var webFS embed.FS

// Version 是当前程序版本，随 /api/info 返回并展示在界面 / 托盘。
const Version = "0.3.0"

// Server 是一个 FileDrop 实例。
type Server struct {
	Port   int
	Dir    string
	NoAuth bool

	token string
	ip    string
	hub   *eventHub
	mu    sync.Mutex // 保护上传落盘 / 重命名 / 文件管理
}

// New 创建实例并生成配对令牌；接收目录不可写时自动回退到用户目录下的 FileDrop。
func New(port int, dir string, noAuth bool) *Server {
	// 优先使用上次保存的接收目录（配置文件与 exe 同级）
	if cfg, err := loadConfig(); err == nil && strings.TrimSpace(cfg.Dir) != "" {
		dir = cfg.Dir
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		if home, e := os.UserHomeDir(); e == nil {
			alt := filepath.Join(home, "FileDrop")
			if e2 := os.MkdirAll(alt, 0o755); e2 == nil {
				dir = alt
			}
		}
	}
	srv := &Server{Port: port, Dir: dir, NoAuth: noAuth, token: randHex(16), ip: lanIP(), hub: newEventHub()}
	if abs, e := filepath.Abs(dir); e == nil {
		srv.Dir = abs
	}
	return srv
}

// Token 返回本次运行生成的配对令牌。
func (s *Server) Token() string { return s.token }

// IP 返回探测到的局域网地址。
func (s *Server) IP() string { return s.ip }

// URL 返回带令牌的连接地址（手机扫码用）。
func (s *Server) URL() string {
	return fmt.Sprintf("http://%s:%d/?t=%s", s.ip, s.Port, s.token)
}

// Handler 返回完整的 HTTP 路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/", s.apiHandler)
	mux.Handle("/", s.fileServer())
	return mux
}

// ListenAndServe 阻塞监听；托盘等场景可在 goroutine 中调用。
func (s *Server) ListenAndServe() error {
	srv := &http.Server{
		Addr:              fmt.Sprintf("0.0.0.0:%d", s.Port),
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       0, // 大文件传输不限时
		WriteTimeout:      0,
	}
	return srv.ListenAndServe()
}

// ---------- 路由 ----------
func (s *Server) apiHandler(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/info":
		jsonOK(w, map[string]any{"ip": s.ip, "port": s.Port, "url": s.URL(), "version": Version})
	case "/api/files":
		if r.Method == http.MethodDelete || r.Method == http.MethodPost && r.URL.Query().Has("delete") {
			s.deleteFile(w, r)
		} else {
			s.listFiles(w)
		}
	case "/api/rename":
		s.renameFile(w, r)
	case "/api/qr":
		s.qrHandler(w, r)
	case "/api/upload/status":
		s.uploadStatus(w, r)
	case "/api/upload/chunk":
		s.uploadChunk(w, r)
	case "/api/upload/complete":
		s.uploadComplete(w, r)
	case "/api/uploads":
		s.partialsHandler(w, r)
	case "/api/download":
		s.download(w, r)
	case "/api/events":
		s.eventsHandler(w, r)
	case "/api/settings":
		s.settingsHandler(w, r)
	case "/api/open-folder":
		s.openFolderHandler(w, r)
	case "/api/pick-folder":
		s.pickFolderHandler(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) fileServer() http.Handler {
	mime.AddExtensionType(".js", "application/javascript; charset=utf-8")
	mime.AddExtensionType(".html", "text/html; charset=utf-8")
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		return http.NotFoundHandler()
	}
	fsrv := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 必须用 path.Clean，不能用 filepath.Clean：
		// URL 路径分隔符恒为正斜杠，而 filepath 在 Windows 上会把 "/app.js" 清成
		// "\app.js"，embed.FS 因此打开失败，静态资源被 SPA 回退成 index.html
		// ——结果是页面只剩 HTML、JS 从未加载，整个界面是死的。
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p != "" && p != "." {
			if f, e := sub.Open(p); e != nil {
				// 带扩展名的是静态资源，找不到就该 404，别悄悄塞回首页掩盖问题
				if path.Ext(p) != "" {
					http.NotFound(w, r)
					return
				}
				r.URL.Path = "/" // SPA 回退
			} else {
				_ = f.Close()
			}
		}
		fsrv.ServeHTTP(w, r)
	})
}

// ---------- 鉴权 ----------
// canWrite 判定写操作是否被允许：关闭鉴权 / 令牌匹配 / 本机回环 任一即可。
// 本机回环豁免让「电脑自己用 localhost 打开」也能完整控制，同时局域网设备仍需令牌。
func (s *Server) canWrite(r *http.Request) bool {
	if s.NoAuth {
		return true
	}
	if t := r.URL.Query().Get("t"); t != "" && t == s.token {
		return true
	}
	return isLoopback(r)
}

func isLoopback(r *http.Request) bool {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ---------- 接口实现 ----------
func (s *Server) listFiles(w http.ResponseWriter) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "read dir")
		return
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".part") ||
			strings.HasSuffix(e.Name(), ".sha256") || strings.HasSuffix(e.Name(), ".bits") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		rec := map[string]any{"name": e.Name(), "size": fi.Size(), "mtime": fi.ModTime().UnixMilli()}
		if b, err := os.ReadFile(filepath.Join(s.Dir, e.Name()) + ".sha256"); err == nil {
			rec["sha256"] = strings.TrimSpace(string(b))
		}
		out = append(out, rec)
	}
	// 新收到的排前面：按修改时间倒序，同时刻则按名称稳定兜底
	sort.Slice(out, func(i, j int) bool {
		mi := out[i]["mtime"].(int64)
		mj := out[j]["mtime"].(int64)
		if mi != mj {
			return mi > mj
		}
		return out[i]["name"].(string) < out[j]["name"].(string)
	})
	jsonOK(w, out)
}

// deleteFile 删除接收目录中的文件及其附属清单 / 残留分块。
func (s *Server) deleteFile(w http.ResponseWriter, r *http.Request) {
	if !s.canWrite(r) {
		jsonErr(w, http.StatusForbidden, "token required")
		return
	}
	name := safeName(r.URL.Query().Get("name"))
	if name == "" {
		jsonErr(w, http.StatusBadRequest, "bad name")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := filepath.Join(s.Dir, name)
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		jsonErr(w, http.StatusInternalServerError, "remove")
		return
	}
	_ = os.Remove(p + ".sha256")
	// 残留分块文件名形如 <name>.<size>.part，需要按前缀匹配清理（连带位图）
	if _, err := s.purgePartialsLocked(name); err != nil {
		jsonErr(w, http.StatusInternalServerError, "purge partials")
		return
	}
	s.hub.broadcast(map[string]any{"type": "files"})
	jsonOK(w, map[string]any{"ok": true})
}

// renameFile 重命名已接收文件（含 SHA-256 清单同步迁移）。
func (s *Server) renameFile(w http.ResponseWriter, r *http.Request) {
	if !s.canWrite(r) {
		jsonErr(w, http.StatusForbidden, "token required")
		return
	}
	var body struct {
		Name    string `json:"name"`
		NewName string `json:"newName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, http.StatusBadRequest, "bad json")
		return
	}
	old, nn := safeName(body.Name), safeName(body.NewName)
	if old == "" || nn == "" || old == nn {
		jsonErr(w, http.StatusBadRequest, "bad name")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	from := filepath.Join(s.Dir, old)
	to := filepath.Join(s.Dir, nn)
	if _, err := os.Stat(from); err != nil {
		jsonErr(w, http.StatusNotFound, "not found")
		return
	}
	if _, err := os.Stat(to); err == nil {
		jsonErr(w, http.StatusConflict, "target exists")
		return
	}
	if err := os.Rename(from, to); err != nil {
		jsonErr(w, http.StatusInternalServerError, "rename")
		return
	}
	if _, err := os.Stat(from + ".sha256"); err == nil {
		_ = os.Rename(from+".sha256", to+".sha256")
	}
	s.hub.broadcast(map[string]any{"type": "files"})
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	name := safeName(r.URL.Query().Get("name"))
	if name == "" {
		http.Error(w, "bad name", http.StatusBadRequest)
		return
	}
	final := filepath.Join(s.Dir, name)
	f, err := os.Open(final)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.Error(w, "stat", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Disposition", contentDisposition(name))
	w.Header().Set("Accept-Ranges", "bytes")
	http.ServeContent(w, r, name, fi.ModTime(), f) // 自动处理 Range / 206
}

// contentDisposition 生成 RFC 6266 的 Content-Disposition 值：
//   attachment; filename="<ascii 兜底>"; filename*=UTF-8''<百分号编码>
//
// 中文文件名如果只写 filename="中文.mp4"，安卓/iOS 浏览器会拿到乱码，
// 必须同时给出 ASCII 兜底名与 filename* 的 UTF-8 编码形式。
// 顺带剔除引号、反斜杠与控制字符，避免文件名撑破 header 结构。
func contentDisposition(name string) string {
	var cleaned strings.Builder
	for _, r := range name {
		if r == '"' || r == '\\' || r < 0x20 || r == 0x7f {
			continue
		}
		cleaned.WriteRune(r)
	}
	n := cleaned.String()
	if n == "" {
		n = "download"
	}
	// ASCII 兜底名：非 ASCII 一律 '_'，供不认 filename* 的老客户端
	ascii := strings.Map(func(r rune) rune {
		if r > 0x7f {
			return '_'
		}
		return r
	}, n)
	// filename* 按 RFC 5987 对 UTF-8 字节做百分号编码
	var enc strings.Builder
	for _, b := range []byte(n) {
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') ||
			b == '-' || b == '.' || b == '_' || b == '~' {
			enc.WriteByte(b)
		} else {
			fmt.Fprintf(&enc, "%%%02X", b)
		}
	}
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, ascii, enc.String())
}

func (s *Server) qrHandler(w http.ResponseWriter, r *http.Request) {
	text := r.URL.Query().Get("text")
	if text == "" {
		text = s.URL()
	}
	size := 256
	if v := r.URL.Query().Get("size"); v != "" {
		if n, e := strconv.Atoi(v); e == nil && n >= 64 && n <= 1024 {
			size = n
		}
	}
	png, err := qrcode.Encode(text, qrcode.Medium, size)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "qr encode")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

// ---------- 工具 ----------
func shaOf(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 1<<20)
	for {
		n, e := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return ""
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func safeName(s string) string {
	s = filepath.Base(s)
	s = strings.ReplaceAll(s, "/", "")
	s = strings.ReplaceAll(s, "\\", "")
	if s == "." || s == "" {
		return ""
	}
	return s
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func lanIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}
	candidates := make([]string, 0)
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() {
			continue
		}
		ip4 := ipnet.IP.To4()
		if ip4 == nil {
			continue
		}
		ip := ip4.String()
		if strings.HasPrefix(ip, "169.254.") {
			continue
		}
		if strings.HasPrefix(ip, "192.168.") || strings.HasPrefix(ip, "10.") || strings.HasPrefix(ip, "172.") {
			return ip
		}
		candidates = append(candidates, ip)
	}
	if len(candidates) > 0 {
		return candidates[0]
	}
	return "127.0.0.1"
}

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	jsonErrCode(w, code, map[string]any{"error": msg})
}

func jsonErrCode(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}