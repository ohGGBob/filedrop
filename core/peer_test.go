package core

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 这些测试一律把 RemoteAddr 设成 203.0.113.x（RFC 5737 的文档地址，非回环）：
// 回环豁免会让「到底是不是授权起了作用」这件事测不出来。

const peerTestHost = "203.0.113.9"
const peerTestIP = peerTestHost + ":4444"

func peerReq(method, target string, body string, remote string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.RemoteAddr = remote
	return r
}

type handshakeResult struct {
	code int
	body string
}

// withShortWait 把「等对方点按钮」的上限压到几秒：真等 90 秒的测试只会拖垮 CI。
func withShortWait(t *testing.T, d time.Duration) {
	t.Helper()
	old := handshakeWait
	handshakeWait = d
	t.Cleanup(func() { handshakeWait = old })
}

// runHandshake 在后台发起「请求上传」，它会长挂等本机批准。
func runHandshake(s *Server, remote string) chan handshakeResult {
	ch := make(chan handshakeResult, 1)
	go func() {
		w := httptest.NewRecorder()
		s.handshakeHandler(w, peerReq(http.MethodPost, "/api/peer/handshake?name=小白手机", "", remote))
		ch <- handshakeResult{code: w.Code, body: w.Body.String()}
	}()
	return ch
}

func waitHandshake(t *testing.T, ch chan handshakeResult) handshakeResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(15 * time.Second):
		t.Fatal("handshake 既没被批准也没超时，卡在里面了")
		return handshakeResult{}
	}
}

func waitPending(t *testing.T, s *Server, want int) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if len(pendingIPs(t, s)) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("待批准队列停在 %d 条，期望 %d 条", len(pendingIPs(t, s)), want)
}

func decide(s *Server, ip string, ok bool) *httptest.ResponseRecorder {
	flag := "0"
	if ok {
		flag = "1"
	}
	w := httptest.NewRecorder()
	target := fmt.Sprintf("/api/peer/decide?t=%s&ip=%s&ok=%s", s.Token(), ip, flag)
	s.decideHandler(w, peerReq(http.MethodPost, target, "", peerTestIP))
	return w
}

func pendingIPs(t *testing.T, s *Server) []map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	s.pendingHandler(w, peerReq(http.MethodGet, "/api/peer/pending?t="+s.Token(), "", peerTestIP))
	if w.Code != http.StatusOK {
		t.Fatalf("列待批准请求失败 %d: %s", w.Code, w.Body.String())
	}
	var out []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestPeerApprovalGrantsWrite 走完一条真实链路：
// 请求 → 本机可见待批准 → 允许 → 拿到授权 → 从同一来源 IP 传分块成功。
func TestPeerApprovalGrantsWrite(t *testing.T) {
	withShortWait(t, 5*time.Second)
	s := newPeerServer(t)
	ch := runHandshake(s, peerTestIP)

	// 批准之前，同一台主机的写请求必须被拦住
	w := httptest.NewRecorder()
	s.uploadChunk(w, peerReq(http.MethodPost, "/api/upload/chunk?name=x.bin&index=0&size=5", "hello", peerTestIP))
	if w.Code != http.StatusForbidden {
		t.Fatalf("未批准的局域网主机竟然能写，返回 %d", w.Code)
	}

	waitPending(t, s, 1)
	seen := pendingIPs(t, s)
	if seen[0]["ip"] != peerTestHost || seen[0]["name"] != "小白手机" {
		t.Errorf("待批准条目内容不对: %+v", seen[0])
	}

	if dw := decide(s, peerTestHost, true); dw.Code != http.StatusOK {
		t.Fatalf("批准失败 %d: %s", dw.Code, dw.Body.String())
	}
	res := waitHandshake(t, ch)
	if res.code != http.StatusOK {
		t.Fatalf("批准后 handshake 应返回授权，实际 %d: %s", res.code, res.body)
	}
	var grant struct {
		Grant     string `json:"grant"`
		URL       string `json:"url"`
		ExpiresMS int64  `json:"expires_ms"`
	}
	if err := json.Unmarshal([]byte(res.body), &grant); err != nil {
		t.Fatal(err)
	}
	if len(grant.Grant) != 32 || !strings.Contains(grant.URL, "?g="+grant.Grant) {
		t.Fatalf("授权内容不对: %+v", grant)
	}
	if grant.ExpiresMS <= time.Now().UnixMilli() {
		t.Errorf("授权到期时间应在将来: %d", grant.ExpiresMS)
	}
	waitPending(t, s, 0)

	w = httptest.NewRecorder()
	s.uploadChunk(w, peerReq(http.MethodPost,
		"/api/upload/chunk?g="+grant.Grant+"&name=x.bin&index=0&size=5", "hello", peerTestIP))
	if w.Code != http.StatusOK {
		t.Fatalf("带着授权仍被拒 %d: %s", w.Code, w.Body.String())
	}

	// 同一个授权换个来源 IP 就不能用，否则「批准时绑 IP」形同虚设
	w = httptest.NewRecorder()
	s.uploadChunk(w, peerReq(http.MethodPost,
		"/api/upload/chunk?g="+grant.Grant+"&name=y.bin&index=0&size=5", "hello", "203.0.113.99:5555"))
	if w.Code != http.StatusForbidden {
		t.Fatalf("授权被别的 IP 复用了，返回 %d", w.Code)
	}

	// 伪造的授权串也不行
	w = httptest.NewRecorder()
	s.uploadChunk(w, peerReq(http.MethodPost,
		"/api/upload/chunk?g=deadbeefdeadbeefdeadbeefdeadbeef&name=z.bin&index=0&size=5", "hello", peerTestIP))
	if w.Code != http.StatusForbidden {
		t.Fatalf("伪造授权竟然通过，返回 %d", w.Code)
	}
}

// TestPeerRepeatHandshakeSkipsPrompt 同一台已批准的设备再来一次，不该再折腾主人一遍。
func TestPeerRepeatHandshakeSkipsPrompt(t *testing.T) {
	withShortWait(t, 5*time.Second)
	s := newPeerServer(t)

	ch := runHandshake(s, peerTestIP)
	waitPending(t, s, 1)
	decide(s, peerTestHost, true)
	first := waitHandshake(t, ch)
	if first.code != http.StatusOK {
		t.Fatalf("第一次批准应成功: %d %s", first.code, first.body)
	}

	second := waitHandshake(t, runHandshake(s, peerTestIP))
	if second.code != http.StatusOK {
		t.Fatalf("已批准的设备第二次应免确认: %d %s", second.code, second.body)
	}
	if len(pendingIPs(t, s)) != 0 {
		t.Error("免确认路径不该留下新的待批准条目")
	}
}

func TestPeerDenyAndTimeout(t *testing.T) {
	withShortWait(t, 300*time.Millisecond)
	s := newPeerServer(t)

	ch := runHandshake(s, peerTestIP)
	waitPending(t, s, 1)
	if dw := decide(s, peerTestHost, false); dw.Code != http.StatusOK {
		t.Fatalf("拒绝这个动作本身应成功: %d %s", dw.Code, dw.Body.String())
	}
	res := waitHandshake(t, ch)
	if res.code != http.StatusForbidden {
		t.Fatalf("被拒绝应返回 403，实际 %d: %s", res.code, res.body)
	}

	// 没人点按钮 → 超时，并且不留悬挂的待批准条目
	res = waitHandshake(t, runHandshake(s, "203.0.113.77:1234"))
	if res.code != http.StatusRequestTimeout {
		t.Fatalf("无人确认应返回 408，实际 %d: %s", res.code, res.body)
	}
	waitPending(t, s, 0)
}

// TestSupersededHandshakeKeepsNewerPending 是双实例实测抓出来的坑：
// 同一台设备连点两次「请求上传」，后来者会顶掉先来者；先来者收尾时若按 IP 删，
// 会把后来者那条待批准一起删掉——屏幕上不再出现按钮，后来者只能干等到超时。
func TestSupersededHandshakeKeepsNewerPending(t *testing.T) {
	withShortWait(t, 5*time.Second)
	s := newPeerServer(t)

	first := runHandshake(s, peerTestIP)
	waitPending(t, s, 1)
	second := runHandshake(s, peerTestIP)

	// 被顶掉的那条应立刻有结果（403），而不是占着队列
	if res := waitHandshake(t, first); res.code != http.StatusForbidden {
		t.Fatalf("被后一个请求顶掉应返回 403，实际 %d: %s", res.code, res.body)
	}
	// 先来者收尾之后，后来者那条必须还在
	waitPending(t, s, 1)
	if dw := decide(s, peerTestHost, true); dw.Code != http.StatusOK {
		t.Fatalf("批准失败 %d: %s", dw.Code, dw.Body.String())
	}
	res := waitHandshake(t, second)
	if res.code != http.StatusOK || !strings.Contains(res.body, `"grant"`) {
		t.Fatalf("后一个请求应被正常批准，实际 %d: %s", res.code, res.body)
	}
	waitPending(t, s, 0)
}

// TestGrantExpires 授权到期后必须立刻失去写能力。
func TestGrantExpires(t *testing.T) {
	s := newPeerServer(t)
	key, _ := s.access.issue(peerTestHost)
	if !s.access.valid(key, peerTestHost) {
		t.Fatal("刚发出的授权就该不可用？")
	}
	s.access.mu.Lock()
	s.access.grants[key].exp = time.Now().Add(-time.Second)
	s.access.mu.Unlock()
	if s.access.valid(key, peerTestHost) {
		t.Fatal("过期授权仍然可用")
	}

	w := httptest.NewRecorder()
	s.uploadChunk(w, peerReq(http.MethodPost,
		"/api/upload/chunk?g="+key+"&name=x.bin&index=0&size=5", "hello", peerTestIP))
	if w.Code != http.StatusForbidden {
		t.Fatalf("过期授权应被拒，实际 %d", w.Code)
	}
}

// TestGrantCountCapped 每次批准都留一条记录，这张表不能无限增长。
func TestGrantCountCapped(t *testing.T) {
	s := newPeerServer(t)
	var latest, latestIP string
	for i := 0; i < maxGrants+6; i++ {
		latestIP = fmt.Sprintf("203.0.113.%d", i+1)
		latest, _ = s.access.issue(latestIP)
	}
	s.access.mu.Lock()
	n := len(s.access.grants)
	s.access.mu.Unlock()
	if n > maxGrants {
		t.Fatalf("授权数 %d 超过上限 %d", n, maxGrants)
	}
	if !s.access.valid(latest, latestIP) {
		t.Error("最新一次批准不该被挤掉")
	}
}

// TestPeerControlEndpointsNeedToken 查看待批准 / 批准 / 代发起请求都要本机写权限，
// 否则任何同网主机都能替屏幕前的人按下「允许」。
func TestPeerControlEndpointsNeedToken(t *testing.T) {
	s := newPeerServer(t)
	cases := []struct {
		name   string
		target string
	}{
		{"GET /api/peer/pending", "/api/peer/pending"},
		{"POST /api/peer/decide", "/api/peer/decide?ip=203.0.113.9&ok=1"},
		{"POST /api/peer/request", "/api/peer/request?to=203.0.113.10:28080"},
		{"POST /api/peers（手动添加）", "/api/peers?to=203.0.113.10:28080"},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		s.apiHandler(w, peerReq(http.MethodPost, c.target, "", peerTestIP))
		if w.Code != http.StatusForbidden {
			t.Errorf("%s 无令牌返回 %d，期望 403", c.name, w.Code)
		}
	}
}

func TestValidPeerTarget(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"192.168.1.20:28080", true},
		{"[fe80::1]:28080", true},
		{"192.168.1.20", false},              // 没端口
		{"printer.local:28080", false},       // 域名不放行：这不是内网探测器
		{"192.168.1.20:0", false},            //
		{"192.168.1.20:99999", false},        //
		{"192.168.1.20:abc", false},          //
		{"http://192.168.1.20:28080", false}, //
		{"", false},                          //
	}
	for _, c := range cases {
		if got := validPeerTarget(c.in); got != c.want {
			t.Errorf("validPeerTarget(%q) = %v, 期望 %v", c.in, got, c.want)
		}
	}
}

// TestPeersEndpointReadable 列设备是读操作，不该卡令牌；而且列表里不许出现任何凭证。
func TestPeersEndpointReadable(t *testing.T) {
	s := newPeerServer(t)
	s.peers.upsert(Peer{ID: "abc", Name: "客厅台式机", Host: "192.168.1.30", Port: 28080,
		URL: "http://192.168.1.30:28080/", Ver: "0.4.0"}, "lan")
	w := httptest.NewRecorder()
	s.apiHandler(w, peerReq(http.MethodGet, "/api/peers", "", peerTestIP))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/peers 无令牌返回 %d，期望 200", w.Code)
	}
	var out struct {
		Peers []Peer `json:"peers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Peers) != 1 || out.Peers[0].Name != "客厅台式机" {
		t.Fatalf("peer 列表不对: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), s.Token()) {
		t.Fatal("设备列表里泄漏了配对令牌")
	}
}

// TestManualAddPeerNeedsRealFileDrop 手填地址要先探测对方 /api/info，不是 FileDrop 就不收。
func TestManualAddPeerNeedsRealFileDrop(t *testing.T) {
	s := newPeerServer(t)

	notFileDrop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"nothing":"here"}`))
	}))
	defer notFileDrop.Close()
	w := httptest.NewRecorder()
	s.apiHandler(w, peerReq(http.MethodPost,
		"/api/peers?t="+s.Token()+"&to="+strings.TrimPrefix(notFileDrop.URL, "http://"), "", peerTestIP))
	if w.Code == http.StatusOK {
		t.Fatalf("对面不是 FileDrop 也收了: %s", w.Body.String())
	}

	real := httptest.NewServer(s.Handler())
	defer real.Close()
	w = httptest.NewRecorder()
	s.apiHandler(w, peerReq(http.MethodPost,
		"/api/peers?t="+s.Token()+"&to="+strings.TrimPrefix(real.URL, "http://"), "", peerTestIP))
	if w.Code != http.StatusOK {
		t.Fatalf("手填真地址应登记成功: %d %s", w.Code, w.Body.String())
	}
	if len(s.peers.list()) == 0 {
		t.Fatal("登记之后列表里却没有")
	}
}

// TestBeaconRoundTripOnRealSocket 用真 UDP socket 压一遍收发：无关流量要没反应、
// whois 要有回音、收到的 iam 要进表、自己的 iam 不能把自己认成对端。
func TestBeaconRoundTripOnRealSocket(t *testing.T) {
	s := newPeerServer(t)
	s.StartDiscovery()
	if s.discErr != nil {
		t.Skipf("端口 %d 绑不上（多半是已有 FileDrop 实例在跑）：%v", discPort, s.discErr)
	}
	defer s.disc.Close()

	client, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	target := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: discPort}

	// 与本机无关的 UDP 流量：不该换来任何回复
	if _, err := client.WriteToUDP([]byte("some other app's traffic"), target); err != nil {
		t.Fatal(err)
	}
	if _, err := client.WriteToUDP(mustMarshalBeacon(t, beacon{M: discMagic, T: "whois"}), target); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2048)
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, _, err := client.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("发 whois 之后没收到自我介绍: %v", err)
	}
	var got beacon
	if err := json.Unmarshal(buf[:n], &got); err != nil {
		t.Fatalf("回复不是合法 beacon: %v / %q", err, string(buf[:n]))
	}
	if got.M != discMagic || got.T != "iam" || got.Port != s.Port || got.ID != s.id {
		t.Errorf("自我介绍内容不对: %+v", got)
	}
	if strings.Contains(string(buf[:n]), s.Token()) {
		t.Fatal("信标里出现了配对令牌")
	}

	// 别人的 iam → 进表
	fake := beacon{M: discMagic, T: "iam", ID: "deadbeef", Name: "另一台", Host: "192.168.1.77", Port: 28080, Ver: "0.4.0"}
	if _, err := client.WriteToUDP(mustMarshalBeacon(t, fake), target); err != nil {
		t.Fatal(err)
	}
	var list []Peer
	for i := 0; i < 200; i++ {
		if list = s.peers.list(); len(list) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(list) != 1 || list[0].Name != "另一台" || list[0].URL != "http://192.168.1.77:28080/" {
		t.Fatalf("收到的 iam 没进表: %+v", list)
	}
	if list[0].Via != "lan" {
		t.Errorf("发现来源应记为 lan，实际 %q", list[0].Via)
	}

	// 自己的 iam 不能把自己当成对端
	if _, err := client.WriteToUDP(mustMarshalBeacon(t, s.selfBeacon("iam")), target); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := s.peers.list(); len(got) != 1 || got[0].ID != "deadbeef" {
		t.Errorf("把自己的广播认成了别人: %+v", got)
	}
}

func mustMarshalBeacon(t *testing.T, b beacon) []byte {
	t.Helper()
	out, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
