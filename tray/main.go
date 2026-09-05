//go:build windows

// filedrop-tray —— Windows 系统托盘版（自包含：内嵌前端与服务端，不依赖外部文件）
package main

import (
	"bytes"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"filedrop/core"

	"github.com/getlantern/systray"
)

func main() {
	port := flag.Int("port", 28080, "监听端口")
	dir := flag.String("dir", "received", "接收文件保存目录")
	flag.Parse()

	// 以可执行文件所在目录为基准，保证 received/ 路径稳定
	if exe, e := os.Executable(); e == nil {
		if d := filepath.Dir(exe); d != "" {
			_ = os.Chdir(d)
		}
	}

	srv := core.New(*port, *dir, false)
	go func() {
		if err := srv.ListenAndServe(); err != nil {
			fmt.Fprintln(os.Stderr, "服务启动失败:", err)
			systray.Quit()
		}
	}()

	systray.Run(func() { onReady(srv) }, onExit)
}

func onReady(srv *core.Server) {
	systray.SetIcon(makeIcon())
	systray.SetTitle("FileDrop")
	systray.SetTooltip("局域网文件快传 v" + core.Version + " · " + srv.URL())

	mOpen := systray.AddMenuItem("打开传输页面", "在浏览器打开 "+srv.URL())
	mCopy := systray.AddMenuItem("复制连接地址", "把带令牌的连接地址复制到剪贴板")
	mFolder := systray.AddMenuItem("打开接收文件夹", "在资源管理器中打开接收目录")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("退出", "停止服务并退出")

	// 服务起来后自动开一次浏览器（页面里有二维码）
	go func() {
		time.Sleep(600 * time.Millisecond)
		openBrowser(srv.URL())
	}()

	go func() {
		for {
			select {
			case <-mOpen.ClickedCh:
				openBrowser(srv.URL())
			case <-mCopy.ClickedCh:
				copyToClipboard(srv.URL())
			case <-mFolder.ClickedCh:
				openFolder(srv.Dir())
			case <-mQuit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
}

func onExit() {}

func openBrowser(u string) {
	cmd := exec.Command("cmd", "/c", "start", "", u)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Run()
}

// copyToClipboard 把文本放进 Windows 剪贴板（经 PowerShell Set-Clipboard，无尾随换行）。
func copyToClipboard(s string) {
	esc := strings.ReplaceAll(s, "'", "''")
	cmd := exec.Command("powershell", "-NoProfile", "-Command", "Set-Clipboard -Value '"+esc+"'")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Run()
}

// openFolder 在资源管理器中打开目录。
func openFolder(dir string) {
	cmd := exec.Command("explorer", dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Start()
}

// makeIcon 生成纯色 PNG 并封装进 ICO（Vista+ 支持 PNG-in-ICO）
func makeIcon() []byte {
	const s = 256
	img := image.NewRGBA(image.Rect(0, 0, s, s))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{47, 109, 240, 255}}, image.Point{}, draw.Src)
	var pb bytes.Buffer
	if err := png.Encode(&pb, img); err != nil {
		return nil
	}
	pngData := pb.Bytes()
	hdr := []byte{0, 0, 1, 0, 1, 0, 0, 0, 0, 0, 1, 0, 32, 0}
	hdr = append(hdr, le32(uint32(len(pngData)))...)
	hdr = append(hdr, le32(22)...)
	return append(hdr, pngData...)
}

func le32(v uint32) []byte {
	return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}
}
