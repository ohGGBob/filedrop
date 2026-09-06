package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Config 是持久化的用户设置（存放在 exe 同级的 filedrop-config.json）。
type Config struct {
	Dir string `json:"dir"`
	// Token 持久化的配对令牌：重启不变，手机书签 / 记住的设备才长期有效。
	Token string `json:"token,omitempty"`
	// LastPeer 最近一次主动连接的对端页面（含令牌或授权）。
	// 安卓 App 收到系统分享时拿它当默认上传目标，相册一步直达。
	LastPeer string `json:"lastPeer,omitempty"`
}

// cfgBase 是配置/数据文件的落点目录。桌面版留空（继续用 exe 同级）；
// 安卓下 os.Executable 不可靠，App 在 Start 前把可写的私有目录传进来。
var cfgBase string

// SetConfigBase 指定配置与数据文件的存放目录。
func SetConfigBase(d string) {
	if strings.TrimSpace(d) != "" {
		cfgBase = d
	}
}

// dataPath 返回随程序一起存放的数据文件路径：便携版拷走目录即带走全部状态。
// 便签刻意放在接收目录之外——否则它会混进 /api/files 的下载列表，
// 还会被「删除文件」之类的操作误伤。
func dataPath(name string) string {
	if cfgBase != "" {
		return filepath.Join(cfgBase, name)
	}
	if exe, err := os.Executable(); err == nil {
		if d := filepath.Dir(exe); d != "" {
			return filepath.Join(d, name)
		}
	}
	return name
}

// configPath 返回配置文件路径。
func configPath() string { return dataPath("filedrop-config.json") }

func loadConfig() (Config, error) {
	b, err := os.ReadFile(configPath())
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, err
	}
	return c, nil
}

func saveConfig(c Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(), b, 0o644)
}

// LoadConfigExport 导出一份当前配置（mobile 桥接层读取 LastPeer 用）。
func LoadConfigExport() Config {
	c, _ := loadConfig()
	return c
}

// peerURLRe 收敛「可记住的对端地址」：只认 http://IP:端口/?t= 或 ?g= 凭据。
// lastPeer 会被 App 拿来直接发文件，绝不能让任意字符串混进配置。
var peerURLRe = regexp.MustCompile(`^http://(\d{1,3}\.){3}\d{1,3}:\d+/\?(?:t|g)=[0-9a-fA-F]+$`)

// RememberPeer 记住最近一次连接的对端页面地址（含凭据）。
func (s *Server) RememberPeer(u string) error {
	u = strings.TrimSpace(u)
	if !peerURLRe.MatchString(u) {
		return errors.New("对端地址格式不合法")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, err := loadConfig()
	if err != nil {
		cfg = Config{}
	}
	cfg.LastPeer = u
	return saveConfig(cfg)
}

// rememberPeerHandler 处理 POST /api/remember-peer?url=...。
func (s *Server) rememberPeerHandler(w http.ResponseWriter, r *http.Request) {
	if !s.canWrite(r) {
		jsonErr(w, http.StatusForbidden, "token required")
		return
	}
	if err := s.RememberPeer(r.URL.Query().Get("url")); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	jsonOK(w, map[string]any{"ok": true})
}

// SetDir 切换接收目录：建目录、校验可写、立即生效并写入配置。
//
// 有未完成的上传时拒绝切换。原因不是洁癖：分块请求各自按当时的目录拼 .part 路径，
// 切目录会把一次上传的 .part 与位图劈到两个目录下，而「中断的传输」面板只扫当前
// 目录——旧目录里那几 GB 残留就此变成看不见也清不掉的孤儿。
// 检查与赋值都放在 s.mu 内：每个分块落盘都持着这把锁，所以持锁扫描到「没有 .part」
// 再切换，中间不会插进新的分块。
func (s *Server) SetDir(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return errors.New("目录为空")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return err
	}
	probe := filepath.Join(abs, ".fdwriteprobe")
	f, err := os.Create(probe)
	if err != nil {
		return err
	}
	f.Close()
	_ = os.Remove(probe)

	s.mu.Lock()
	defer s.mu.Unlock()
	// 先读出整份配置再改 Dir：只写 Config{Dir} 会把 Token / LastPeer 一并抹掉，
	// 手机上的书签和 App 记住的设备就全失效了。
	cfg, err := loadConfig()
	if err != nil {
		cfg = Config{}
	}
	if abs == s.Dir() {
		cfg.Dir = abs
		return saveConfig(cfg) // 切回当前目录：只记住，不动别的
	}
	if left := len(scanPartialsIn(s.Dir())); left > 0 {
		return fmt.Errorf("还有 %d 个未传完的文件，请先在「中断的传输」里续传或清理，再切换接收目录", left)
	}
	s.setDir(abs)
	cfg.Dir = abs
	return saveConfig(cfg)
}

// settingsHandler 读取 / 修改接收目录。
func (s *Server) settingsHandler(w http.ResponseWriter, r *http.Request) {
	// GET 会返回本机接收目录的绝对路径，属于敏感信息，读也要鉴权（2026-09-06）
	if r.Method != http.MethodPost && !s.canWrite(r) {
		jsonErr(w, http.StatusForbidden, "token required")
		return
	}
	if r.Method == http.MethodPost {
		if !s.canWrite(r) {
			jsonErr(w, http.StatusForbidden, "token required")
			return
		}
		var body struct {
			Dir string `json:"dir"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Dir) == "" {
			jsonErr(w, http.StatusBadRequest, "参数错误")
			return
		}
		if err := s.SetDir(body.Dir); err != nil {
			jsonErr(w, http.StatusBadRequest, "无法使用该目录："+err.Error())
			return
		}
		s.hub.broadcast(map[string]any{"type": "files"})
	}
	jsonOK(w, map[string]any{"dir": s.Dir(), "config": configPath()})
}

// openFolderHandler 在 Windows 资源管理器里打开接收目录。
func (s *Server) openFolderHandler(w http.ResponseWriter, r *http.Request) {
	if !s.canWrite(r) {
		jsonErr(w, http.StatusForbidden, "token required")
		return
	}
	d := s.Dir()
	switch runtime.GOOS {
	case "windows":
		_ = exec.Command("explorer", d).Start()
	case "darwin":
		_ = exec.Command("open", d).Start()
	default:
		_ = exec.Command("xdg-open", d).Start()
	}
	jsonOK(w, map[string]any{"ok": true, "dir": d})
}

// pickFolderHandler 弹出 Windows 原生文件夹选择对话框，返回所选路径。
// 仅在 Windows 下真实生效；其他平台返回 400。
func (s *Server) pickFolderHandler(w http.ResponseWriter, r *http.Request) {
	if !s.canWrite(r) {
		jsonErr(w, http.StatusForbidden, "token required")
		return
	}
	if runtime.GOOS != "windows" {
		jsonErr(w, http.StatusBadRequest, "文件夹选择器仅支持 Windows")
		return
	}
	script := `Add-Type -AssemblyName System.Windows.Forms; ` +
		`$d = New-Object System.Windows.Forms.FolderBrowserDialog; ` +
		`$d.Description = '选择 FileDrop 接收文件的目录'; ` +
		`if ($d.ShowDialog() -eq 'OK') { Write-Output $d.SelectedPath }`

	ctx, cancel := context.WithTimeout(r.Context(), 180*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-STA", "-WindowStyle", "Hidden", "-Command", script)
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		jsonErr(w, 500, "打开选择器失败："+err.Error())
		return
	}
	p := strings.TrimSpace(string(out))
	if p == "" {
		jsonOK(w, map[string]any{"cancelled": true})
		return
	}
	jsonOK(w, map[string]any{"path": p})
}
