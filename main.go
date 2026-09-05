// FileDrop —— 局域网文件快传（无界面命令行版）
// 真正的业务逻辑在 core 包；本文件只负责参数解析与启动打印。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"filedrop/core"
)

func main() {
	port := flag.Int("port", 28080, "监听端口")
	dir := flag.String("dir", "received", "接收文件保存目录")
	noAuth := flag.Bool("no-auth", false, "关闭配对令牌（仅本地测试用）")
	flag.Parse()

	// 以可执行文件所在目录为基准，保证 received/ 路径稳定
	if exe, e := os.Executable(); e == nil {
		if d := filepath.Dir(exe); d != "" {
			_ = os.Chdir(d)
		}
	}

	srv := core.New(*port, *dir, *noAuth)
	if *noAuth {
		fmt.Println("[warn] 已关闭配对令牌，任何同网设备都能上传")
	}
	fmt.Println("FileDrop 已启动  v" + core.Version)
	fmt.Println("  本机连接地址: " + srv.URL())
	fmt.Println("  接收目录:     " + filepath.Clean(srv.Dir))
	fmt.Println("  按 Ctrl+C 停止")

	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "服务启动失败:", err)
		os.Exit(1)
	}
}
