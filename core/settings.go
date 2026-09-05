package core

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Config 是持久化的用户设置（存放在 exe 同级的 filedrop-config.json）。
type Config struct {
	Dir string `json:"dir"`
}

// configPath 返回配置文件路径：与可执行文件同级，保证「便携版」拷贝目录即走。
func configPath() string {
	name := "filedrop-config.json"
	if exe, err := os.Executable(); err == nil {
		if d := filepath.Dir(exe); d != "" {
			return filepath.Join(d, name)
		}
	}
	return name
}

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

// SetDir 切换接收目录：建目录、校验可写、立即生效并写入配置。
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
	s.Dir = abs
	s.mu.Unlock()
	return saveConfig(Config{Dir: abs})
}

// settingsHandler 读取 / 修改接收目录。
func (s *Server) settingsHandler(w http.ResponseWriter, r *http.Request) {
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
	jsonOK(w, map[string]any{"dir": s.Dir, "config": configPath()})
}

// openFolderHandler 在 Windows 资源管理器里打开接收目录。
func (s *Server) openFolderHandler(w http.ResponseWriter, r *http.Request) {
	if !s.canWrite(r) {
		jsonErr(w, http.StatusForbidden, "token required")
		return
	}
	s.mu.Lock()
	d := s.Dir
	s.mu.Unlock()
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
