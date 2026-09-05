package core

import "testing"

// isolateConfig 让单个测试使用独立的配置文件目录。
//
// 背景：New 现在会把令牌持久化到配置文件，SetDir / RememberPeer 也会写它。
// 测试若不隔离，就会读写真实的 filedrop-config.json，既污染用户配置，
// 又让测试之间通过「上一位留下的 cfg.Dir」互相串台（单跑通过、全跑失败的典型元凶）。
func isolateConfig(t *testing.T) {
	t.Helper()
	old := cfgBase
	cfgBase = t.TempDir()
	t.Cleanup(func() { cfgBase = old })
}

// newPeerServer 是 peer_test 们共用的构造器：隔离配置 + 指定端口与临时接收目录。
func newPeerServer(t *testing.T) *Server {
	t.Helper()
	isolateConfig(t)
	return New(28123, t.TempDir(), false)
}
