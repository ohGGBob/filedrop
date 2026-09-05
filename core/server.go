// Package core 是 FileDrop 的服务端核心：HTTP 路由 + 分块上传落盘 + 下载 + 文件管理。
// 前端静态资源通过 go:embed 编进二进制，因此成品是真正的单文件可执行程序。
package core

import (
	"context"
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
	"net/url"
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
const Version = "0.5.0"

// Server 是一个 FileDrop 实例。
type Server struct {
	Port   int
	NoAuth bool

	// dir 受 dirMu 保护，只能通过 Dir() / setDir() 访问。
	// 目录会被「设置」接口随时改写，而列表 / 下载 / 托盘菜单同时在读，
	// 直接用公开字段就是数据竞争（-race 下可见）。
	dirMu sync.RWMutex
	dir   string

	token string
	ip    string
	hub   *eventHub
	mu    sync.Mutex // 保护上传落盘 / 重命名 / 文件管理

	// notesMu 单独保护便签文件。它和 s.mu 管的是互不相干的东西，
	// 分开放就不会出现「写一段便签要等某个 GB 级分块落盘」。
	notesMu sync.Mutex

	// 设备发现：id 是本实例的随机标识（重启即换，避免旧列表里认错了机器），
	// deviceName 用主机名，只为了在别人的列表里看得懂这是哪台。
	id         string
	deviceName string
	peers      *peerTable
	access     *peerAccess
	disc       *net.UDPConn
	discDone   chan struct{}
	discErr    error // 发现通道没开成时，界面要好话说清为什么

	// httpSrv 由 ListenAndServe 记录，供 Shutdown 优雅停机用。
	httpSrv *http.Server
}

// New 创建实例并生成配对令牌；接收目录不可写时自动回退到用户目录下的 FileDrop。
func New(port int, dir string, noAuth bool) *Server {
	// 优先使用上次保存的接收目录（配置文件与 exe 同级）
	if cfg, err := loadConfig(); err == nil && strings.TrimSpace(cfg.Dir) != "" {
		dir = cfg.Dir
	}
	if abs, e := filepath.Abs(dir); e == nil {
		dir = abs
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		if home, e := os.UserHomeDir(); e == nil {
			alt := filepath.Join(home, "FileDrop")
			if e2 := os.MkdirAll(alt, 0o755); e2 == nil {
				dir = alt
			}
		}
	}
	return &Server{
		Port: port, dir: dir, NoAuth: noAuth, token: randHex(16), ip: lanIP(), hub: newEventHub(),
		id: randHex(8), deviceName: deviceNameOf(),
		peers: newPeerTable(), access: newPeerAccess(),
	}
}

// Dir 返回当前接收目录的快照。
//
// dirMu 是叶子锁：它内部绝不获取 s.mu，因此上传处理函数可以在持有 s.mu 时安全调用
// （锁序恒为 s.mu → dirMu，不会倒置死锁）。
// 单次操作应在开头取一次快照并全程使用——中途被切目录会让同一次上传的
// .part 与位图落到两个目录里。
func (s *Server) Dir() string {
	s.dirMu.RLock()
	defer s.dirMu.RUnlock()
	return s.dir
}

// setDir 切换接收目录；调用方需保证目录已创建且可写。
func (s *Server) setDir(d string) {
	s.dirMu.Lock()
	s.dir = d
	s.dirMu.Unlock()
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
	s.StartDiscovery()
	srv := &http.Server{
		Addr:              fmt.Sprintf("0.0.0.0:%d", s.Port),
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       0, // 大文件传输不限时
		WriteTimeout:      0,
	}
	s.httpSrv = srv
	return srv.ListenAndServe()
}

// Shutdown 优雅停机：关掉监听与全部连接、停掉发现广播。
// 安卓 App 在界面销毁时调用，避免服务残留在后台占着端口。
func (s *Server) Shutdown() error {
	s.stopDiscovery()
	if srv := s.httpSrv; srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return srv.Shutdown(ctx)
	}
	return nil
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
	case "/api/notes":
		s.notesHandler(w, r)
	case "/api/peers":
		s.peersHandler(w, r)
	case "/api/peer/handshake":
		s.handshakeHandler(w, r)
	case "/api/peer/pending":
		s.pendingHandler(w, r)
	case "/api/peer/decide":
		s.decideHandler(w, r)
	case "/api/peer/request":
		s.requestHandler(w, r)
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
	// 对端批准过的临时授权：绑来源 IP 且会过期，比终身有效的配对令牌收敛。
	if g := r.URL.Query().Get("g"); g != "" && s.access.valid(g, peerIP(r)) {
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
	dir := s.Dir()
	entries, err := os.ReadDir(dir)
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
		if b, err := os.ReadFile(filepath.Join(dir, e.Name()) + ".sha256"); err == nil {
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
		badName(w)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := filepath.Join(s.Dir(), name)
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
	if old == "" || nn == "" {
		badName(w)
		return
	}
	if old == nn {
		jsonErr(w, http.StatusBadRequest, "新文件名与原文件名相同")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	from := filepath.Join(s.Dir(), old)
	to := filepath.Join(s.Dir(), nn)
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
		http.Error(w, "invalid file name", http.StatusBadRequest)
		return
	}
	final := filepath.Join(s.Dir(), name)
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
//
//	attachment; filename="<ascii 兜底>"; filename*=UTF-8''<百分号编码>
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

// ownURL 判断 text 是否是一个指向本机的 http(s) 地址。
// 二维码接口只接受这种输入（见 qrHandler 的注释）。
func (s *Server) ownURL(text string) bool {
	u, err := url.Parse(text)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	h := u.Hostname()
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || h == s.ip
}

func (s *Server) qrHandler(w http.ResponseWriter, r *http.Request) {
	// 早期版本把 ?text= 里的任意字符串直接编成二维码，且不限长度——
	// 同网任何人拿它当免费的钓鱼二维码生成器（扫一下就跳到外站），
	// 超长文本还能白白消耗编码开销。现在只允许指向本机的地址，其余回退到连接地址。
	text := strings.TrimSpace(r.URL.Query().Get("text"))
	if text == "" || len(text) > 512 || !s.ownURL(text) {
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

// maxNameBytes 限制文件名长度：落盘时还要追加 <size>.part 与 .bits 后缀，
// 而 NTFS 单个路径成分上限是 255。超长的名字与其在底层 CreateFile 撞出一个
// 看不懂的 500，不如在入口就说清楚。
const maxNameBytes = 200

// winReserved 是 Windows 保留设备名（不含 COM10+ / LPT10+，现代系统不保留）。
var winReserved = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true, "CLOCK$": true}

// safeName 把客户端给的任意字符串收敛成一个安全的纯文件名，返回 "" 表示不可用。
//
// 五道关卡，缺一不可：
//  1. 只取 basename 并剔掉分隔符，挡掉 ../../etc/passwd 式穿越。注意
//     filepath.Base("..") 返回的仍是 ".."，必须显式拒绝，否则
//     filepath.Join(dir, "..") 直接指到接收目录的上一级去。
//  2. 拒绝 Windows 保留设备名：打开 dir\NUL 会成功并读出无限零字节，
//     下载处理函数就此卡死、白占一个连接（CON / COM1 / nul.txt 同理）。
//  3. 拒绝结尾的点与空格——NTFS 根本建不出这样的文件。
//  4. 限制长度。
//  5. 清洗 Windows 文件名非法字符 < > : " | ? * 与控制字符。这些字符在
//     macOS / 安卓上合法，直接落盘会撞出两种更糟的结果：CreateFile 报错变成
//     看不懂的 500；或 `a:b` 被当成 NTFS 交替数据流——接口返回 200，字节却写
//     进 a 这个文件的数据流里，目录中只留下一个空的 a。就地换成下划线，
//     complete 会把真实文件名回给前端显示。
func safeName(s string) string {
	s = filepath.Base(s)
	s = strings.ReplaceAll(s, "/", "")
	s = strings.ReplaceAll(s, "\\", "")
	if s == "." || s == ".." || s == "" {
		return ""
	}
	if strings.HasSuffix(s, ".") || strings.HasSuffix(s, " ") {
		return ""
	}
	if len(s) > maxNameBytes {
		return ""
	}
	s = sanitizeIllegalChars(s)
	stem := s // 设备名看第一个点之前的部分："nul.txt" 在 Windows 上同样是设备
	if i := strings.IndexByte(stem, '.'); i >= 0 {
		stem = stem[:i]
	}
	stem = strings.ToUpper(stem)
	if winReserved[stem] {
		return ""
	}
	if (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && len(stem) == 4 {
		if d := stem[3]; d >= '1' && d <= '9' {
			return ""
		}
	}
	return s
}

// sanitizeIllegalChars 把 Windows 不允许出现在文件名里的字符换成下划线。
// 非法字符全是 ASCII，而 UTF-8 多字节序列的每个字节都 >= 0x80，
// 所以逐字节扫描不会拆坏中文文件名。
func sanitizeIllegalChars(s string) string {
	const illegal = `<>:"|?*`
	b := []byte(s)
	hit := false
	for i, c := range b {
		if c < 0x20 || c == 0x7f || strings.IndexByte(illegal, c) >= 0 {
			b[i] = '_'
			hit = true
		}
	}
	if !hit {
		return s
	}
	return string(b)
}

// badName 统一「文件名不可用」的响应文案，供各写接口复用。
func badName(w http.ResponseWriter) {
	jsonErr(w, http.StatusBadRequest, "文件名无效：不能为空或 ..，不能用 Windows 保留名（NUL/CON/COM1 等），不能以点或空格结尾")
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
		if !ok {
			continue
		}
		ip4 := ipnet.IP.To4()
		if ip4 == nil || ip4.IsLoopback() {
			continue
		}
		if ip4.IsLinkLocalUnicast() { // 169.254.x 是 DHCP 拿不到地址时的废地址
			continue
		}
		// 必须用 IsPrivate：它按 CIDR 判定 10/8、172.16/12、192.168/16。
		// 手写 HasPrefix(ip, "172.") 会把公网的 172.5.6.7 也当成局域网地址，
		// 生成的二维码就把手机指向一个访问不通的地址。
		if ip4.IsPrivate() {
			return ip4.String()
		}
		candidates = append(candidates, ip4.String())
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
