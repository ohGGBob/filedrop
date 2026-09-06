package core

// 局域网设备发现 + 「对端批准」式临时写授权。
//
// 发现走 UDP 广播，beacon 里只有主机名 / ip / 端口 / 版本，**刻意不带配对令牌**：
// 令牌存在的唯一意义就是「防同 WiFi 其他人乱传」，把它塞进任何主机都能收下的广播里，
// 等于把这层防护直接取消。所以对端想用写接口，必须经过人在屏幕前点一次「允许」，
// 换回一个绑源 IP、会过期的临时授权（见 grantTable）——而不是一辈子的令牌。

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	discPort     = 45581            // 所有实例共用这个发现端口，才能互相听到
	discMagic    = "filedrop/1"     // 包前缀，用来把无关的 UDP 流量一眼扔掉
	discInterval = 3 * time.Second  // 广播节奏
	peerTTL      = 15 * time.Second // 三个周期没听到就当对方下线
	grantTTL     = 30 * time.Minute // 一次批准能写多久
	maxGrants    = 8                // 同时存活的授权数上限
	maxPending   = 16               // 待批准队列上限
)

// handshakeWait 是「请求上传」挂住等对方点按钮的最长时间。
// 做成变量是为了让单测不必真等 90 秒。
var handshakeWait = 90 * time.Second

// beacon 是发现报文。iam = 自我介绍，whois = 问有谁在。
type beacon struct {
	M    string `json:"m"`
	T    string `json:"t"`
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	Host string `json:"host,omitempty"`
	Port int    `json:"port,omitempty"`
	Ver  string `json:"ver,omitempty"`
}

// Peer 是一台发现到的同类设备。
type Peer struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Host string `json:"host"`
	Port int    `json:"port"`
	URL  string `json:"url"`
	Ver  string `json:"version"`
	// Via 说明它是怎么被知道的：lan（广播听到）还是 manual（手填地址探测成功）。
	Via  string `json:"via"`
	Seen int64  `json:"seen_ms"`
}

// peerTable 是发现到的设备集合。广播丢包、乱序都属正常，所以只记「最后见到时间」，
// 过期即淘汰，不做任何确认握手。
type peerTable struct {
	mu sync.Mutex
	m  map[string]*Peer // key = 对端实例 id
}

func newPeerTable() *peerTable { return &peerTable{m: make(map[string]*Peer)} }

// upsert 记下一次见到；返回 true 表示这是第一次见到这台设备（值得推 SSE 刷新）。
func (t *peerTable) upsert(p Peer, via string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	p.Seen = time.Now().UnixMilli()
	p.Via = via
	if old, ok := t.m[p.ID]; ok {
		*old = p
		return false
	}
	t.m[p.ID] = &p
	return true
}

// prune 淘汰超时条目；返回是否有变化。
func (t *peerTable) prune() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	cutoff := time.Now().Add(-peerTTL).UnixMilli()
	changed := false
	for id, p := range t.m {
		// 手填的设备不淘汰：用户既然专门填过，多半正是广播穿不过去的那台。
		if p.Seen < cutoff && p.Via != "manual" {
			delete(t.m, id)
			changed = true
		}
	}
	return changed
}

func (t *peerTable) list() []Peer {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Peer, 0, len(t.m))
	for _, p := range t.m {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Host < out[j].Host
	})
	return out
}

// ---------- 临时写授权 ----------

type grant struct {
	ip  string
	exp time.Time
}

// pendingReq 是一次「某设备想向我上传」的等待。dec 由本机页面上的批准/拒绝按钮投递。
type pendingReq struct {
	ip   string
	name string
	at   time.Time
	dec  chan bool
}

type peerAccess struct {
	mu      sync.Mutex
	grants  map[string]*grant
	pending map[string]*pendingReq // key = 请求方 ip
}

func newPeerAccess() *peerAccess {
	return &peerAccess{grants: make(map[string]*grant), pending: make(map[string]*pendingReq)}
}

// issue 发一个绑定到 ip 的授权，顺手清掉已过期的，条数封顶。
func (a *peerAccess) issue(ip string) (string, time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for k, g := range a.grants {
		if g.exp.Before(now) {
			delete(a.grants, k)
		}
	}
	for len(a.grants) >= maxGrants {
		oldest, oldestExp := "", time.Now().Add(time.Hour)
		for k, g := range a.grants {
			if g.exp.Before(oldestExp) {
				oldest, oldestExp = k, g.exp
			}
		}
		if oldest == "" {
			break
		}
		delete(a.grants, oldest)
	}
	key := randHex(16)
	exp := now.Add(grantTTL)
	a.grants[key] = &grant{ip: ip, exp: exp}
	return key, exp
}

// valid 判断授权是否可用：未过期且来源 IP 与申请时那个完全相同。
// 绑 IP 是必须的，否则一个授权能被同网任何主机复用，批准就成了摆设。
func (a *peerAccess) valid(key, ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	g, ok := a.grants[key]
	if !ok {
		return false
	}
	if g.exp.Before(time.Now()) {
		delete(a.grants, key)
		return false
	}
	return g.ip == ip
}

// findFor 查某个 IP 是否已被批准过（还没过期）。命中就直接放行，
// 免得同一台手机每次刷新页面都要电脑主人点一次「允许」。
func (a *peerAccess) findFor(ip string) (string, time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for k, g := range a.grants {
		if g.exp.Before(now) {
			delete(a.grants, k)
			continue
		}
		if g.ip == ip {
			return k, g.exp
		}
	}
	return "", time.Time{}
}

func (a *peerAccess) addPending(ip, name string) *pendingReq {
	a.mu.Lock()
	defer a.mu.Unlock()
	if old, ok := a.pending[ip]; ok {
		// 同一台设备连着请求：后一个取代前一个，别让页面堆出一排重复按钮。
		select {
		case old.dec <- false:
		default:
		}
		delete(a.pending, ip)
	}
	if len(a.pending) >= maxPending {
		return nil // 本机主人还没处理完，先不受理
	}
	q := &pendingReq{ip: ip, name: name, at: time.Now(), dec: make(chan bool, 1)}
	a.pending[ip] = q
	return q
}

func (a *peerAccess) takePending(ip string) *pendingReq {
	a.mu.Lock()
	defer a.mu.Unlock()
	q := a.pending[ip]
	delete(a.pending, ip)
	return q
}

// releasePending 只摘掉自己那一条。同一 IP 后来者会顶掉先来者，
// 如果按 IP 无条件删，被顶掉的那条挂起请求收尾时会把新的删掉——
// 后来者就永远不出现在待批准列表里，只能干等到超时。
func (a *peerAccess) releasePending(q *pendingReq) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pending[q.ip] == q {
		delete(a.pending, q.ip)
	}
}

func (a *peerAccess) listPending() []map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]map[string]any, 0, len(a.pending))
	for _, q := range a.pending {
		out = append(out, map[string]any{"ip": q.ip, "name": q.name, "at": q.at.UnixMilli()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["at"].(int64) > out[j]["at"].(int64) })
	return out
}

// ---------- HTTP 接口 ----------

// peerIP 取请求来源 IP。同一台机器上跑两个实例互测时，只要走局域网地址就不是回环，
// 授权路径能被真实验证。
func peerIP(r *http.Request) string {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return host
}

// handshakeHandler 是**被请求方**（B）上的入口：不要求令牌——要令牌就成了先有鸡先有蛋。
// 它把请求挂起，等 B 屏幕上有人点「允许」，批准才回一个临时授权。
func (s *Server) handshakeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	ip := peerIP(r)
	if ip == "" {
		jsonErr(w, http.StatusBadRequest, "bad request")
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))

	if key, exp := s.access.findFor(ip); key != "" {
		jsonOK(w, s.grantReply(key, exp))
		return
	}
	q := s.access.addPending(ip, name)
	if q == nil {
		jsonErr(w, http.StatusServiceUnavailable, "对方屏幕上的待确认请求已满，请先处理几条再试")
		return
	}
	s.hub.broadcast(map[string]any{"type": "peer_request", "ip": ip, "name": name})
	defer s.access.releasePending(q)

	timer := time.NewTimer(handshakeWait)
	defer timer.Stop()
	select {
	case ok := <-q.dec:
		if !ok {
			jsonErr(w, http.StatusForbidden, "对方拒绝了这个请求")
			return
		}
		key, exp := s.access.issue(ip)
		jsonOK(w, s.grantReply(key, exp))
	case <-timer.C:
		jsonErr(w, http.StatusRequestTimeout, "对方没有在规定时间内确认")
	case <-r.Context().Done():
		// 请求方中途关页面 / 断网：静默退出，B 的待批准横幅会自己超时消失。
	}
}

func (s *Server) grantReply(key string, exp time.Time) map[string]any {
	ms := int64(0)
	if !exp.IsZero() {
		ms = exp.UnixMilli()
	}
	// 直接把可访问地址给出去，前端跳过去就是同源操作，不必为跨域开 CORS。
	return map[string]any{
		"grant":      key,
		"url":        fmt.Sprintf("http://%s:%d/?g=%s", s.ip, s.Port, key),
		"expires_ms": ms,
		"ttl_ms":     grantTTL.Milliseconds(),
	}
}

// pendingHandler 让本机页面知道有谁在等着被批准。
func (s *Server) pendingHandler(w http.ResponseWriter, r *http.Request) {
	if !s.canWrite(r) {
		jsonErr(w, http.StatusForbidden, "token required")
		return
	}
	jsonOK(w, s.access.listPending())
}

// decideHandler 是本机点「允许 / 拒绝」的落点。它要求本机写权限：
// 否则任何同网主机都能替屏幕前的人按下「允许」。
func (s *Server) decideHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if !s.canWrite(r) {
		jsonErr(w, http.StatusForbidden, "token required")
		return
	}
	ip := r.URL.Query().Get("ip")
	if ip == "" {
		jsonErr(w, http.StatusBadRequest, "bad params")
		return
	}
	ok := r.URL.Query().Get("ok") == "1"
	q := s.access.takePending(ip)
	if q == nil {
		jsonErr(w, http.StatusNotFound, "请求已过期")
		return
	}
	select {
	case q.dec <- ok:
	default:
	}
	s.hub.broadcast(map[string]any{"type": "peer_request"})
	jsonOK(w, map[string]any{"ok": ok})
}

// requestHandler 是**请求方**（A）上的入口：由本机服务端去敲 B 的 handshake，
// 这样浏览器的请求始终同源，跨域问题根本不出场。
func (s *Server) requestHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if !s.canWrite(r) {
		jsonErr(w, http.StatusForbidden, "token required")
		return
	}
	to := strings.TrimSpace(r.URL.Query().Get("to"))
	if !validPeerTarget(to) {
		jsonErr(w, http.StatusBadRequest, "地址应形如 192.168.1.20:28080")
		return
	}
	u := fmt.Sprintf("http://%s/api/peer/handshake?name=%s", to, url.QueryEscape(s.deviceName))
	client := &http.Client{Timeout: handshakeWait + 15*time.Second}
	req, err := http.NewRequest(http.MethodPost, u, strings.NewReader(""))
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "build request")
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		jsonErr(w, http.StatusBadGateway, "联系不上对方："+err.Error())
		return
	}
	defer resp.Body.Close()
	var out struct {
		Grant     string `json:"grant"`
		URL       string `json:"url"`
		ExpiresMS int64  `json:"expires_ms"`
		Error     string `json:"error"`
	}
	if e := json.NewDecoder(resp.Body).Decode(&out); e != nil {
		jsonErr(w, http.StatusBadGateway, "对方返回了无法解析的内容")
		return
	}
	if resp.StatusCode != http.StatusOK || out.Grant == "" {
		msg := out.Error
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		jsonErr(w, http.StatusBadGateway, msg)
		return
	}
	// 顺手把这个对端记进列表，跳过去之后至少看得到它是谁。
	s.peers.upsert(Peer{ID: "manual:" + to, Name: to, Host: hostOf(to), Port: portOf(to),
		URL: "http://" + to + "/", Ver: "-", Via: "manual"}, "manual")
	jsonOK(w, out)
}

// validPeerTarget 只接受「IP 字面量:端口」。放域名进去就等于让本机去访问任意主机，
// 这个接口已经是写接口级别的权限，不必再多一个内网探测器。
func validPeerTarget(s string) bool {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return false
	}
	n, err := strconv.Atoi(port)
	if err != nil || n <= 0 || n > 65535 {
		return false
	}
	return net.ParseIP(host) != nil
}

// peersHandler 列出发现到的同类设备，附本机信息与发现通道状态。
// 读接口，不强制令牌：里面没有任何凭证，只有一个局域网地址。
func (s *Server) peersHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.addPeerHandler(w, r)
		return
	}
	self := map[string]any{"name": s.deviceName, "id": s.id, "port": s.Port, "ip": s.ip}
	discErr := ""
	if s.discErr != nil {
		discErr = s.discErr.Error()
	}
	jsonOK(w, map[string]any{
		"self": self, "peers": s.peers.list(), "listening": s.disc != nil, "discover_error": discErr,
	})
}

// addPeerHandler 手动登记一台设备：探测 http://<host:port>/api/info，
// 确认对面真是 FileDrop 才收进列表。广播被 AP / 防火墙挡住时，这是唯一能走通的路。
func (s *Server) addPeerHandler(w http.ResponseWriter, r *http.Request) {
	if !s.canWrite(r) {
		jsonErr(w, http.StatusForbidden, "token required")
		return
	}
	to := strings.TrimSpace(r.URL.Query().Get("to"))
	if !validPeerTarget(to) {
		jsonErr(w, http.StatusBadRequest, "地址应形如 192.168.1.20:28080")
		return
	}
	client := &http.Client{Timeout: 5 * time.Second}
	u := fmt.Sprintf("http://%s/api/info", to)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "build request")
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		jsonErr(w, http.StatusBadGateway, "联系不上对方："+err.Error())
		return
	}
	defer resp.Body.Close()
	var info struct {
		IP      string `json:"ip"`
		Port    int    `json:"port"`
		Version string `json:"version"`
	}
	if e := json.NewDecoder(resp.Body).Decode(&info); e != nil || info.Port == 0 {
		jsonErr(w, http.StatusBadGateway, "对面不像是一个 FileDrop 服务")
		return
	}
	s.peers.upsert(Peer{
		ID: "manual:" + to, Name: to, Host: hostOf(to), Port: portOf(to),
		URL: "http://" + to + "/", Ver: info.Version, Via: "manual",
	}, "manual")
	s.hub.broadcast(map[string]any{"type": "peers"})
	jsonOK(w, map[string]any{"ok": true, "peers": s.peers.list()})
}

// ---------- UDP 广播 ----------

// StartDiscovery 打开发现通道。失败（端口被占 / 防火墙拦）只是没有自动发现，
// 文件传输本身完全不受影响，所以这里绝不阻断启动。
func (s *Server) StartDiscovery() {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: discPort})
	if err != nil {
		s.discErr = err
		return
	}
	s.disc = conn
	done := make(chan struct{})
	s.discDone = done
	go s.readBeacons(conn)
	go s.beaconLoop(conn, done)
}

// stopDiscovery 关闭发现通道并让广播 goroutine 退出。App 停机时调用。
func (s *Server) stopDiscovery() {
	if s.discDone != nil {
		close(s.discDone)
		s.discDone = nil
	}
	if s.disc != nil {
		_ = s.disc.Close()
		s.disc = nil
	}
}

func (s *Server) selfBeacon(t string) beacon {
	return beacon{M: discMagic, T: t, ID: s.id, Name: s.deviceName, Host: s.ip, Port: s.Port, Ver: Version}
}

// beaconLoop 定期广播自我介绍；每隔一个周期顺带问一次「有谁在」，
// 这样刚启动的实例不必等到下一轮才有对端可听。
func (s *Server) beaconLoop(conn *net.UDPConn, done <-chan struct{}) {
	tick := time.NewTicker(discInterval)
	defer tick.Stop()
	n := 0
	for {
		select {
		case <-done: // 停机：通道已关，别再广播
			return
		case <-tick.C:
		}
		n++
		s.sendAll(conn, s.selfBeacon("iam"))
		if n%2 == 0 {
			s.sendAll(conn, beacon{M: discMagic, T: "whois"})
		}
		if s.peers.prune() {
			s.hub.broadcast(map[string]any{"type": "peers"})
		}
	}
}

// sendAll 发到全局广播、定向广播、组播及已知对端单播，多播互补提升穿透。
// 组播 224.0.0.251 (mDNS) / 239.255.255.250 (SSDP) 在部分 AP 屏蔽广播但放行组播时仍可达。
func (s *Server) sendAll(conn *net.UDPConn, b beacon) {
	blob, err := json.Marshal(b)
	if err != nil {
		return
	}
	targets := []string{"255.255.255.255", "224.0.0.251", "239.255.255.250"}
	if ip := net.ParseIP(s.ip).To4(); ip != nil {
		targets = append(targets, fmt.Sprintf("%d.%d.%d.255", ip[0], ip[1], ip[2]))
	}
	for _, p := range s.peers.list() {
		if p.Host != s.ip {
			targets = append(targets, p.Host)
		}
	}
	seen := make(map[string]bool, len(targets))
	for _, t := range targets {
		if seen[t] {
			continue
		}
		seen[t] = true
		// 组播/广播需设置 TTL，避免被路由器过度扩散（本地链路即可）
		if t == "224.0.0.251" || t == "239.255.255.250" {
			_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
			// 尝试以组播方式发出，失败则回退普通 WriteTo
			if addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", t, discPort)); err == nil {
				_, _ = conn.WriteToUDP(blob, addr)
				continue
			}
		}
		_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_, _ = conn.WriteToUDP(blob, &net.UDPAddr{IP: net.ParseIP(t), Port: discPort})
	}
}

// readBeacons 收包并回话。SetReadDeadline 是为了让 Close 之后（或包一直不来时）
// 这个 goroutine 有明确的退出路径，而不是永远堵在 ReadFromUDP 上。
func (s *Server) readBeacons(conn *net.UDPConn) {
	buf := make([]byte, 2048)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(discInterval * 3))
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return // socket 已关
		}
		var b beacon
		if e := json.Unmarshal(buf[:n], &b); e != nil || b.M != discMagic {
			continue
		}
		switch b.T {
		case "whois":
			blob, _ := json.Marshal(s.selfBeacon("iam"))
			_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
			_, _ = conn.WriteToUDP(blob, from)
		case "iam":
			if b.ID == "" || b.ID == s.id || b.Port <= 0 {
				continue
			}
			// host 来自网络，会被拼成界面里的可点链接；只收 IP 字面量，
			// 免得一个伪造信标把「打开」指向任意主机。局域网信标本来就报 IP。
			if net.ParseIP(b.Host) == nil {
				continue
			}
			isNew := s.peers.upsert(Peer{
				ID: b.ID, Name: b.Name, Host: b.Host, Port: b.Port,
				URL: fmt.Sprintf("http://%s:%d/", b.Host, b.Port), Ver: b.Ver,
			}, "lan")
			if isNew {
				s.hub.broadcast(map[string]any{"type": "peers"})
			}
		}
	}
}

func hostOf(hostPort string) string {
	h, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		return hostPort
	}
	return h
}

func portOf(hostPort string) int {
	_, p, err := net.SplitHostPort(hostPort)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(p)
	return n
}

// deviceNameOf 取主机名第一段作为设备名：DESKTOP-A1B2C3.local → DESKTOP-A1B2C3。
func deviceNameOf() string {
	h, err := os.Hostname()
	if err != nil || strings.TrimSpace(h) == "" {
		return "本机"
	}
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	return h
}
