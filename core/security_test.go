package core

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	return New(0, t.TempDir(), true) // NoAuth：测试里不折腾令牌
}

// TestSafeName 覆盖 v0.4 收紧的四类非法名：穿越、Windows 保留名、结尾点/空格、超长。
func TestSafeName(t *testing.T) {
	cases := []struct {
		in   string
		want string // "" 表示必须拒绝
	}{
		{"report.pdf", "report.pdf"},
		{"中文 照片 #1.MP4", "中文 照片 #1.MP4"},
		{"a-b_c.d.tar.gz", "a-b_c.d.tar.gz"},

		{"..", ""},                     // filepath.Base("..") == ".."，不挡就是上级目录
		{"../..", ""},                  //
		{".", ""},                      //
		{"", ""},                       //
		{"   ", ""},                    // 只剩空白
		{"name.", ""},                  // NTFS 建不出结尾点
		{"name.txt.", ""},              //
		{"name.txt ", ""},              // 结尾空格同理
		{strings.Repeat("x", 300), ""}, // 加上 .<size>.part 后缀会顶破 NTFS 单成分上限

		// Windows 设备名：打开 dir\NUL 会成功并读出无限零字节，卡死下载协程
		{"NUL", ""},
		{"nul.txt", ""},
		{"CON", ""},
		{"PRN.txt", ""},
		{"aux", ""},
		{"COM1", ""},
		{"com9.mp4", ""},
		{"LPT1", ""},
		{"CLOCK$", ""},
	}
	for _, c := range cases {
		if got := safeName(c.in); got != c.want {
			t.Errorf("safeName(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

// TestSafeNameKeepsNonReservedDevicePrefixes COM10+ / LPT10+ 在现代 Windows 上不是保留名。
func TestSafeNameKeepsNonReservedDevicePrefixes(t *testing.T) {
	for _, n := range []string{"COM10.log", "LPT99.txt", "note.txt", "null.c", "assist.exe", "printer.pdf"} {
		if got := safeName(n); got != n {
			t.Errorf("safeName(%q) = %q，不该误伤正常文件名", n, got)
		}
	}
}

// TestReservedNameDownloadDoesNotHang ?name=NUL 若被放行，http.ServeContent 会
// 从设备流里读出无限零字节——所以这条测试带超时，卡住即失败。
func TestReservedNameDownloadDoesNotHang(t *testing.T) {
	srv := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	for _, name := range []string{"NUL", "nul.txt", "COM1", ".."} {
		resp, err := client.Get(ts.URL + "/api/download?name=" + urlEncode(name))
		if err != nil {
			t.Fatalf("下载 ?name=%s 失败（疑似卡在设备流上）: %v", name, err)
		}
		code := resp.StatusCode
		resp.Body.Close()
		if code != http.StatusBadRequest {
			t.Errorf("下载 ?name=%s 返回 %d，期望 400", name, code)
		}
	}
}

// TestUniqueTarget 验证「同名不同内容不再覆盖」。
func TestUniqueTarget(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p+".sha256", []byte(shaOf(p)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("movie.mp4", "旧内容")
	oldSum := shaOf(filepath.Join(dir, "movie.mp4"))

	t.Run("同名同内容判重复并复用旧文件", func(t *testing.T) {
		p, name, reused := uniqueTarget(dir, "movie.mp4", oldSum, int64(len("旧内容")))
		if !reused || name != "movie.mp4" {
			t.Fatalf("reused=%v name=%q，期望复用 movie.mp4", reused, name)
		}
		if p != filepath.Join(dir, "movie.mp4") {
			t.Errorf("目标路径 = %q", p)
		}
	})

	t.Run("同名不同内容另存不覆盖", func(t *testing.T) {
		p, name, reused := uniqueTarget(dir, "movie.mp4", "deadbeef", int64(len("完全不同的新内容")))
		if reused {
			t.Fatal("内容不同却判成重复")
		}
		if name != "movie (1).mp4" {
			t.Errorf("name = %q，期望 movie (1).mp4", name)
		}
		if filepath.Base(p) != "movie (1).mp4" {
			t.Errorf("path = %q", p)
		}
		// 旧文件必须还在
		if _, err := os.Stat(filepath.Join(dir, "movie.mp4")); err != nil {
			t.Errorf("旧文件被弄丢了: %v", err)
		}
	})

	t.Run("序号继续沿用资源管理器惯例递增", func(t *testing.T) {
		write("movie (1).mp4", "第二份")
		_, name, _ := uniqueTarget(dir, "movie.mp4", "cafe", int64(len("第三份")))
		if name != "movie (2).mp4" {
			t.Errorf("name = %q，期望 movie (2).mp4", name)
		}
	})

	t.Run("无同名文件时原名落盘", func(t *testing.T) {
		_, name, reused := uniqueTarget(dir, "fresh.bin", "abc", 3)
		if name != "fresh.bin" || reused {
			t.Errorf("name=%q reused=%v", name, reused)
		}
	})
}

// TestQRRejectsForeignText 二维码接口只能编本机地址。
// QR 编码对同一输入是确定性的，所以「外链的 PNG 与默认 PNG 完全相同」
// 就等于证明外链参数被忽略、回退到了本机连接地址。
func TestQRRejectsForeignText(t *testing.T) {
	srv := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	get := func(url string) []byte {
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s -> %d", url, resp.StatusCode)
		}
		var b bytes.Buffer
		if _, err := b.ReadFrom(resp.Body); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}

	def := get(ts.URL + "/api/qr")
	if phish := get(ts.URL + "/api/qr?text=" + urlEncode("https://evil.example/扫码领红包")); !bytes.Equal(def, phish) {
		t.Error("外链 text 未被拒绝：二维码接口仍是公开的钓鱼二维码生成器")
	}
	if long := get(ts.URL + "/api/qr?text=" + urlEncode("http://"+srv.IP()+"/"+strings.Repeat("a", 4000))); !bytes.Equal(def, long) {
		t.Error("超长 text 未被拒绝")
	}
	if own := get(ts.URL + "/api/qr?text=" + urlEncode(srv.URL())); !bytes.Equal(def, own) {
		t.Error("本机连接地址应当正常编码，却被回退了")
	}
}

// TestDirSwitchIsRaceFree 在切目录的同时高频读目录：-race 下若 Dir 仍是裸字段会报数据竞争。
func TestDirSwitchIsRaceFree(t *testing.T) {
	srv := newTestServer(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				rec := httptest.NewRecorder()
				srv.listFiles(rec)
				_ = srv.Dir()
			}
		}()
	}
	target := filepath.Join(t.TempDir(), "elsewhere")
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 50; j++ {
			if err := srv.SetDir(target); err != nil {
				t.Errorf("SetDir: %v", err)
				return
			}
		}
	}()
	wg.Wait()
	if srv.Dir() != filepath.Clean(target) {
		t.Errorf("最终目录 = %q，期望 %q", srv.Dir(), target)
	}
}

// TestSetDirRefusedWhileUploadPending 未完成上传存在时禁止切目录，
// 否则 .part 与位图会被劈到两个目录，旧目录里的残留连面板都扫不到。
func TestSetDirRefusedWhileUploadPending(t *testing.T) {
	srv := newTestServer(t)
	part := partPath(srv.Dir(), "big.bin", 1<<30)
	if err := os.WriteFile(part, []byte("半截数据"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := srv.Dir()
	if err := srv.SetDir(filepath.Join(t.TempDir(), "new")); err == nil {
		t.Fatal("存在 .part 残留时竟然切换成功")
	}
	if srv.Dir() != before {
		t.Errorf("切换失败后目录被改动了: %q -> %q", before, srv.Dir())
	}

	// 清掉残留后就能切了
	if _, err := srv.purgePartials(""); err != nil {
		t.Fatal(err)
	}
	next := filepath.Join(t.TempDir(), "new2")
	if err := srv.SetDir(next); err != nil {
		t.Fatalf("清理后切换仍失败: %v", err)
	}
	if srv.Dir() != filepath.Clean(next) {
		t.Errorf("Dir() = %q，期望 %q", srv.Dir(), next)
	}
}

func urlEncode(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-' || c == '.' || c == '_' || c == '~':
			b.WriteByte(c)
		default:
			const hex = "0123456789ABCDEF"
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xf])
		}
	}
	return b.String()
}
