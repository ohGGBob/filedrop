// fdtest —— FileDrop 自测客户端（实现与前端相同的 v0.2 分块协议）
// 用法:
//
//	go run ./testclient up   <base> <token> <file>
//	go run ./testclient down <base> <name> <out>
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const chunkSize = 8 * 1024 * 1024

func main() {
	if len(os.Args) < 3 {
		fmt.Println("usage:\n  fdtest up <base> <token> <file>\n  fdtest down <base> <name> <out>")
		os.Exit(1)
	}
	switch os.Args[1] {
	case "up":
		if len(os.Args) != 5 {
			fail(fmt.Errorf("up needs <base> <token> <file>"))
		}
		up(os.Args[2], os.Args[3], os.Args[4])
	case "down":
		if len(os.Args) != 5 {
			fail(fmt.Errorf("down needs <base> <name> <out>"))
		}
		down(os.Args[2], os.Args[3], os.Args[4])
	default:
		fail(fmt.Errorf("unknown mode %q", os.Args[1]))
	}
}

type statusResp struct {
	Received  int64  `json:"received"`
	Total     int    `json:"total"`
	ChunkSize int    `json:"chunkSize"`
	Missing   []int  `json:"missing"`
	Complete  bool   `json:"complete"`
	Error     string `json:"error"`
}

func up(base, token, path string) {
	f, err := os.Open(path)
	if err != nil {
		fail(err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		fail(err)
	}
	size := fi.Size()
	name := fi.Name()

	// 1) 查询缺块列表
	var st statusResp
	{
		u := fmt.Sprintf("%s/api/upload/status?name=%s&size=%d", base, url.QueryEscape(name), size)
		resp, e := http.Get(u)
		if e != nil {
			fail(e)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			fail(fmt.Errorf("status HTTP %d: %s", resp.StatusCode, string(body)))
		}
		if e = json.Unmarshal(body, &st); e != nil {
			fail(e)
		}
	}
	buf := make([]byte, chunkSize)
	if st.Complete {
		fmt.Println("已存在同名完整文件，跳过上传")
	} else {
		missing := st.Missing
		fmt.Printf("上传 %s  size=%d  total=%d块  缺 %d 块\n", name, size, st.Total, len(missing))

		sent := 0
		for _, i := range missing {
			off := int64(i) * chunkSize
			end := off + chunkSize
			if end > size {
				end = size
			}
			n, re := f.ReadAt(buf[:end-off], off)
			if re != nil && re != io.EOF {
				fail(re)
			}
			u := fmt.Sprintf("%s/api/upload/chunk?t=%s&name=%s&index=%d&size=%d",
				base, url.QueryEscape(token), url.QueryEscape(name), i, size)
			req, _ := http.NewRequest(http.MethodPost, u, bytes.NewReader(buf[:n]))
			req.Header.Set("Content-Type", "application/octet-stream")
			resp, e2 := http.DefaultClient.Do(req)
			if e2 != nil {
				fail(e2)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				fail(fmt.Errorf("分块 %d 失败 HTTP %d: %s", i, resp.StatusCode, string(body)))
			}
			sent++
			fmt.Printf("\r  已补 %d/%d", sent, len(missing))
		}
		fmt.Println()
	}

	// 2) complete 收尾（可能因并发乱序缺块而重试）
	var sha string
	finalName := name // 服务端可能另存为 "名字 (1).ext"，回环要按真实落盘名下载
	for attempt := 0; attempt < 3; attempt++ {
		u := fmt.Sprintf("%s/api/upload/complete?t=%s&name=%s&size=%d",
			base, url.QueryEscape(token), url.QueryEscape(name), size)
		req, _ := http.NewRequest(http.MethodPost, u, bytes.NewReader(nil))
		resp, e := http.DefaultClient.Do(req)
		if e != nil {
			fail(e)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		if resp.StatusCode == 200 {
			sha, _ = m["sha256"].(string)
			if nn, ok := m["name"].(string); ok && nn != "" {
				finalName = nn
			}
			break
		}
		if mm, ok := m["missing"].([]any); ok && len(mm) > 0 {
			fmt.Printf("  缺 %d 块，重补后再收尾\n", len(mm))
			for _, v := range mm {
				idx, _ := v.(float64)
				i := int(idx)
				off := int64(i) * chunkSize
				end := off + chunkSize
				if end > size {
					end = size
				}
				n, re := f.ReadAt(buf[:end-off], off)
				if re != nil && re != io.EOF {
					fail(re)
				}
				cu := fmt.Sprintf("%s/api/upload/chunk?t=%s&name=%s&index=%d&size=%d",
					base, url.QueryEscape(token), url.QueryEscape(name), i, size)
				req2, _ := http.NewRequest(http.MethodPost, cu, bytes.NewReader(buf[:n]))
				req2.Header.Set("Content-Type", "application/octet-stream")
				resp2, e2 := http.DefaultClient.Do(req2)
				if e2 != nil {
					fail(e2)
				}
				io.Copy(io.Discard, resp2.Body)
				resp2.Body.Close()
			}
			continue
		}
		fail(fmt.Errorf("complete HTTP %d: %s", resp.StatusCode, string(body)))
	}

	local := shaFile(path)
	fmt.Println("服务端 sha256:", sha)
	fmt.Println("本地   sha256:", local)
	if !strings.EqualFold(sha, local) {
		fmt.Println("SHA 不一致 ✗")
		os.Exit(2)
	}
	fmt.Println("SHA 一致 ✓")

	// 3) 下载回环校验
	out := path + ".roundtrip"
	if finalName != name {
		fmt.Println("服务端未覆盖同名文件，另存为:", finalName)
	}
	down(base, finalName, out)
	rt := shaFile(out)
	fmt.Println("回环   sha256:", rt)
	if strings.EqualFold(rt, local) {
		fmt.Println("上传→下载 回环一致 ✓")
		_ = os.Remove(out)
	} else {
		fmt.Println("回环不一致 ✗")
		os.Exit(3)
	}
}

func down(base, name, out string) {
	u := fmt.Sprintf("%s/api/download?name=%s", base, url.QueryEscape(name))
	resp, err := http.Get(u)
	if err != nil {
		fail(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		fail(fmt.Errorf("下载 HTTP %d", resp.StatusCode))
	}
	of, err := os.Create(out)
	if err != nil {
		fail(err)
	}
	defer of.Close()
	h := sha256.New()
	if _, err = io.Copy(io.MultiWriter(of, h), resp.Body); err != nil {
		fail(err)
	}
	fmt.Printf("下载 %s -> %s (sha256 %s)\n", name, out, hex.EncodeToString(h.Sum(nil)))
}

func shaFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		fail(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		fail(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func fail(e error) {
	fmt.Fprintln(os.Stderr, "ERR:", e)
	os.Exit(1)
}
