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
	"net"
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

	// 单实例互斥：同端口已在运行时友好提示而非静默失败
	if !acquireSingleInstance(*port) {
		fmt.Fprintln(os.Stderr, "FileDrop 已在运行中（同端口已被占用），请勿重复启动")
		// 尝试唤起已运行实例的页面
		openBrowser(fmt.Sprintf("http://127.0.0.1:%d/", *port))
		os.Exit(0)
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

func acquireSingleInstance(port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 400*time.Millisecond)
	if err != nil {
		return true // 无人占用，放行
	}
	_ = c.Close()
	// 端口已被占用，判定为已运行
	return false
}

func onReady(srv *core.Server) {
	systray.SetIcon(makeIcon())
	systray.SetTitle("FileDrop")
	systray.SetTooltip("FileDrop v" + core.Version + " · 局域网文件快传 · " + srv.URL())

	mOpen := systray.AddMenuItem("打开传输页面", "在浏览器打开 "+srv.URL())
	mCopy := systray.AddMenuItem("复制连接地址", "把带令牌的连接地址复制到剪贴板")
	mFolder := systray.AddMenuItem("打开接收文件夹", "在资源管理器中打开接收目录")
	mInfo := systray.AddMenuItem("关于 FileDrop v"+core.Version, "局域网文件快传 · 便携商业版 1.0")
	mInfo.Disable()
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

// makeIcon 生成品牌图标：圆角卡片 + 文档 + 向下箭头，辨识度远高于纯色方块
func makeIcon() []byte {
	const s = 256
	img := image.NewRGBA(image.Rect(0, 0, s, s))
	// 背景透明，画圆角卡片
	for y := 0; y < s; y++ {
		for x := 0; x < s; x++ {
			img.Set(x, y, color.RGBA{0, 0, 0, 0})
		}
	}
	// 圆角矩形背景 #2563eb
	br := 42
	for y := 22; y < s-22; y++ {
		for x := 22; x < s-22; x++ {
			dx := 0
			dy := 0
			if x < 22+br && y < 22+br { dx = 22+br - x; dy = 22+br - y }
			if x > s-22-br && y < 22+br { dx = x - (s-22-br); dy = 22+br - y }
			if x < 22+br && y > s-22-br { dx = 22+br - x; dy = y - (s-22-br) }
			if x > s-22-br && y > s-22-br { dx = x - (s-22-br); dy = y - (s-22-br) }
			if dx*dx+dy*dy <= br*br {
				img.Set(x, y, color.RGBA{37, 99, 235, 255})
			} else if dx == 0 && dy == 0 {
				// 矩形中部
				img.Set(x, y, color.RGBA{37, 99, 235, 255})
			}
		}
	}
	// 画白色文档形状
	docColor := color.RGBA{255, 255, 255, 255}
	for y := 66; y < 176; y++ {
		for x := 78; x < 178; x++ {
			if x > 148 && y < 88 && (x-148)+(y-66) > 30 { continue } // 折角
			img.Set(x, y, docColor)
		}
	}
	// 折角阴影
	for y := 66; y < 88; y++ {
		for x := 148; x < 178; x++ {
			if (x-148)+(y-66) > 30 && (x-148)+(y-66) < 34 {
				img.Set(x, y, color.RGBA{191, 219, 254, 255})
			}
		}
	}
	// 向下箭头（蓝）
	arrow := color.RGBA{37, 99, 235, 255}
	for y := 112; y < 158; y++ {
		for x := 124; x < 132; x++ {
			img.Set(x, y, arrow)
		}
	}
	// 箭头尖
	for dy := 0; dy < 12; dy++ {
		for dx := -dy; dx <= dy; dx++ {
			img.Set(128+dx, 150+dy, arrow)
		}
	}
	// 底座
	for y := 164; y < 172; y++ {
		for x := 104; x < 152; x++ {
			img.Set(x, y, arrow)
		}
	}
	// 柔和外发光
	_ = draw.Draw // keep import
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
