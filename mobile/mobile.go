// Package mobile 把 FileDrop 服务端桥接给 Android App（经 gomobile bind 生成 .aar）。
//
// 生成绑定：gomobile bind -target=android -androidapi 26 -o filedrop.aar ./mobile
// 生成的 Java 类为 mobile.Mobile，方法：
//
//	String Mobile.start(long port, String dir) throws Exception  // 启动服务，返回带令牌的本地 URL
//	void   Mobile.stop()                                         // 优雅停机
//	String Mobile.version()                                      // 版本号
//	String Mobile.dir()                                          // 当前接收目录
//
// App 的用法：手机就是一台「主机」，界面(WebView)加载 start 返回的地址，
// 平板 / 电脑扫页面上的二维码连进来即可互传。
package mobile

import (
	"fmt"
	"net"
	"sync"

	"filedrop/core"

	// gomobile bind 生成的胶水代码 import x/mobile/bind。
	// 这里空导入把它钉在 go.mod 里——go mod tidy 会剪掉没被 import 的依赖，
	// 而 CI 上的 bind 步骤少了它就会编译失败。
	_ "golang.org/x/mobile/bind"
)

var (
	mu      sync.Mutex
	current *core.Server
)

// Start 启动内嵌服务端并返回带令牌的本地地址（http://127.0.0.1:PORT/?t=xxx）。
// 已在运行时直接返回现有地址，不重复启动。
func Start(port int64, dir string) (string, error) {
	mu.Lock()
	defer mu.Unlock()
	if current != nil {
		return current.URL(), nil
	}
	if port <= 0 || port > 65535 {
		return "", fmt.Errorf("端口号不合法: %d", port)
	}
	// 先探测端口可用，把「被占用」这类错误当场带回给界面，
	// 而不是等 goroutine 里 ListenAndServe 失败后没人接。
	probe, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return "", fmt.Errorf("端口 %d 被占用: %v", port, err)
	}
	_ = probe.Close()

	srv := core.New(int(port), dir, false)
	go func() { _ = srv.ListenAndServe() }()
	current = srv
	return srv.URL(), nil
}

// Stop 停止服务并释放端口。未启动时是安全的空操作。
func Stop() {
	mu.Lock()
	defer mu.Unlock()
	if current == nil {
		return
	}
	_ = current.Shutdown()
	current = nil
}

// Version 返回 FileDrop 版本号。
func Version() string { return core.Version }

// Dir 返回当前接收目录（可能因配置持久化与传入值不同）。
func Dir() string {
	mu.Lock()
	defer mu.Unlock()
	if current == nil {
		return ""
	}
	return current.Dir()
}
