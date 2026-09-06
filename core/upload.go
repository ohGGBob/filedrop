package core

import (
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 上传协议 v0.4：
//  1. 客户端 GET /api/upload/status 取得缺块列表（依据位图精确断点续传）；
//     查询带上取样指纹 &fp=<16hex>，服务端只有指纹也对得上才敢直接判 complete；
//  2. 客户端并发 POST /api/upload/chunk，服务端按 index*chunkSize WriteAt 落盘并记位；
//  3. 全部完成后 POST /api/upload/complete，服务端校验大小、算 SHA-256、重命名落盘。
//
// 并发友好：分块可乱序 / 并行到达，最终一致性由 complete 步骤保证。

const chunkSize = 8 * 1024 * 1024 // 8 MiB，需与前端 / 测试客户端一致

// chunkCount 返回 size 字节对应总分块数。
func chunkCount(size int64) int {
	return int((size + chunkSize - 1) / chunkSize)
}

// 内容取样指纹：对文件头 / 中 / 尾三个窗口做双 lane FNV-1a，输出 16 位十六进制。
// 它只是「重复上传可以跳过」的提示，不是完整性校验——真正的判定在 complete
// 比对 SHA-256。算法必须与前端 / fdtest 逐字节一致。
const sampleWindow = 64 * 1024

// sampleOffsets 返回三个取样窗口的起点。小文件上窗口会重叠，只要三端算法
// 用同一套偏移，重叠不影响一致性判断。
func sampleOffsets(size int64) [3]int64 {
	half := size / 2
	if half > sampleWindow/2 {
		half -= sampleWindow / 2
	}
	tail := size - sampleWindow
	if tail < 0 {
		tail = 0
	}
	return [3]int64{0, half, tail}
}

func fnv1a(h uint32, b []byte) uint32 {
	for _, c := range b {
		h ^= uint32(c)
		h *= 0x01000193
	}
	return h
}

// fingerprintOf 由三个取样窗口算出指纹；size 参与混合，避免
// 「窗口字节相同但总长不同」被误判为同一份内容。
func fingerprintOf(size int64, windows [3][]byte) string {
	var h1, h2 uint32 = 0x811c9dc5, 0x01000193
	mix := func(b []byte) {
		h1 = fnv1a(h1, b)
		h2 = fnv1a(h2^uint32(len(b)), b)
	}
	var sz [8]byte
	for i := 0; i < 8; i++ {
		sz[i] = byte(uint64(size) >> (8 * i))
	}
	mix(sz[:])
	for _, b := range windows {
		mix(b)
	}
	return fmt.Sprintf("%08x%08x", h1, h2)
}

// fingerprintFile 从已落盘的完整文件算指纹。
func fingerprintFile(path string, size int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var ws [3][]byte
	for i, off := range sampleOffsets(size) {
		n := size - off
		if n > sampleWindow {
			n = sampleWindow
		}
		b := make([]byte, n)
		if _, err := f.ReadAt(b, off); err != nil && err != io.EOF {
			return "", err
		}
		ws[i] = b
	}
	return fingerprintOf(size, ws), nil
}

// 残留文件名编码规则：<原文件名>.<文件字节数>.part[.bits]
//
// 关键点：把 size 编进文件名。否则「同名文件、不同大小」的两次上传会共用同一份
// .part / 位图——旧位图长度恰好够新文件用时会被直接复用，新文件的前 N 块被误判为
// 已收到，最终 complete 算出的 SHA-256 是「新旧拼接后的损坏数据」的哈希，
// 校验因此"通过"，损坏被静默写盘。带上 size 后两次上传天然隔离。
func partPath(dir, name string, size int64) string {
	return filepath.Join(dir, fmt.Sprintf("%s.%d.part", name, size))
}

// bitsPath 返回该文件对应的接收位图路径。
func bitsPath(dir, name string, size int64) string { return partPath(dir, name, size) + ".bits" }

// loadBitmap 读取接收位图（chunkCount(size) 字节，每字节 0/1 表示该块是否已收齐）。
// 位图文件缺失时：若存在 v0.1 顺序上传残留的 .part，按其连续字节推断并视为旧版数据。
func loadBitmap(dir, name string, size int64) []byte {
	total := chunkCount(size)
	bits := make([]byte, total)
	if data, err := os.ReadFile(bitsPath(dir, name, size)); err == nil && len(data) >= total {
		copy(bits, data[:total])
		return bits
	}
	// 迁移旧版（v0.1 顺序上传）残留：.part 前 N 块视为已收
	if fi, err := os.Stat(partPath(dir, name, size)); err == nil {
		n := int((fi.Size() + chunkSize - 1) / chunkSize)
		if n > total {
			n = total
		}
		for i := 0; i < n; i++ {
			bits[i] = 1
		}
	}
	return bits
}

func writeBitmap(dir, name string, size int64, bits []byte) error {
	p := bitsPath(dir, name, size)
	if err := os.WriteFile(p, bits, 0o644); err != nil {
		return err
	}
	// 同步到位图落盘，避免断电丢位图导致重传判断错
	if f, err := os.OpenFile(p, os.O_RDWR, 0o644); err == nil {
		_ = f.Sync()
		_ = f.Close()
	}
	return nil
}

func hasDiskSpace(dir string, need int64) bool {
	const reserve = 64 * 1024 * 1024
	return checkFreeSpace(dir, need+reserve)
}

// missingChunks 返回所有未收到的分块下标。
func missingChunks(bits []byte) []int {
	missing := make([]int, 0)
	for i, b := range bits {
		if b == 0 {
			missing = append(missing, i)
		}
	}
	return missing
}

// contigReceived 返回从 0 起连续收到的字节数（UI 展示用）。
func contigReceived(bits []byte, size int64) int64 {
	for i, b := range bits {
		if b == 0 {
			r := int64(i) * chunkSize
			if r > size {
				r = size
			}
			return r
		}
	}
	return size
}

// uploadStatus 返回断点续传所需的缺块信息。
func (s *Server) uploadStatus(w http.ResponseWriter, r *http.Request) {
	name := safeRelPath(r.URL.Query().Get("name"))
	size, err := strconv.ParseInt(r.URL.Query().Get("size"), 10, 64)
	if name == "" {
		badName(w)
		return
	}
	if err != nil || size <= 0 || size > 20*1024*1024*1024 {
		jsonErr(w, http.StatusBadRequest, "bad params")
		return
	}

	dir := s.Dir()
	lk := s.chunkLock(dir + "|" + name + "|" + strconv.FormatInt(size, 10))
	lk.Lock()
	defer lk.Unlock()

	// 同名同大小的完整文件已存在，且客户端带的取样指纹对得上 → 才敢直接判完成。
	// 只看「同名 + 同大小」不够：那正是「改了内容重新导出一份」最容易撞上的组合，
	// 直接跳过等于把用户刚发的文件悄悄丢掉。
	if fi, e := os.Stat(filepath.Join(dir, name)); e == nil && fi.Size() == size {
		if fp := r.URL.Query().Get("fp"); len(fp) == 16 {
			if got, e := fingerprintFile(filepath.Join(dir, name), size); e == nil && strings.EqualFold(got, fp) {
				jsonOK(w, map[string]any{
					"received": size, "total": chunkCount(size), "chunkSize": chunkSize,
					"missing": []int{}, "complete": true, "name": name,
				})
				return
			}
		}
		// 同名同大小但内容对不上：告诉前端有冲突，由用户决定跳过 / 覆盖 / 共存。
		// 服务端默认继续走位图（等价于"共存"），complete 里同名不同内容会自动改名。
		jsonOK(w, map[string]any{
			"received": 0, "total": chunkCount(size), "chunkSize": chunkSize,
			"missing": missingChunks(loadBitmap(dir, name, size)),
			"complete": false, "exists": true, "name": name,
		})
		return
	}

	bits := loadBitmap(dir, name, size)
	jsonOK(w, map[string]any{
		"received":  contigReceived(bits, size),
		"total":     chunkCount(size),
		"chunkSize": chunkSize,
		"missing":   missingChunks(bits),
		"complete":  false,
	})
}

// uploadChunk 写入单个分块，不做落盘收尾（收尾在 complete）。
func (s *Server) uploadChunk(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := safeRelPath(q.Get("name"))
	size, sErr := strconv.ParseInt(q.Get("size"), 10, 64)
	index, iErr := strconv.Atoi(q.Get("index"))

	if !s.canWrite(r) {
		jsonErr(w, http.StatusForbidden, "token required")
		return
	}
	if name == "" {
		badName(w)
		return
	}
	if sErr != nil || iErr != nil || size <= 0 {
		jsonErr(w, http.StatusBadRequest, "bad params")
		return
	}
	total := chunkCount(size)
	if index < 0 || index >= total {
		jsonErr(w, http.StatusBadRequest, "bad index")
		return
	}
	// 末块允许的最大字节数 = 到文件末尾的余量，避免越界写满一整块
	maxWrite := int64(chunkSize)
	if rem := size - int64(index)*chunkSize; rem < maxWrite {
		maxWrite = rem
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, chunkSize+1))
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "read body")
		return
	}
	if int64(len(body)) > maxWrite {
		jsonErr(w, http.StatusBadRequest, "chunk too large")
		return
	}

	dir := s.Dir()
	lk := s.chunkLock(dir + "|" + name + "|" + strconv.FormatInt(size, 10))
	lk.Lock()
	defer lk.Unlock()

	if !hasDiskSpace(dir, int64(len(body))) {
		jsonErr(w, http.StatusInsufficientStorage, "磁盘空间不足")
		return
	}

	// 文件夹上传的 name 带子路径（"相册/IMG_1.jpg"），先确保子目录存在
	if err := os.MkdirAll(filepath.Dir(partPath(dir, name, size)), 0o755); err != nil {
		jsonErr(w, http.StatusInternalServerError, "mkdir")
		return
	}

	bits := loadBitmap(dir, name, size)
	f, err := os.OpenFile(partPath(dir, name, size), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "open part")
		return
	}
	if _, err = f.WriteAt(body, int64(index)*chunkSize); err != nil {
		f.Close()
		jsonErr(w, http.StatusInternalServerError, "write part")
		return
	}
	_ = f.Sync()
	_ = f.Close()

	bits[index] = 1
	if err := writeBitmap(dir, name, size, bits); err != nil {
		jsonErr(w, http.StatusInternalServerError, "write bitmap")
		return
	}
	jsonOK(w, map[string]any{"ok": true})
}

// sidecarSha 读取 <文件>.sha256 清单；不存在返回 ""。
func sidecarSha(path string) string {
	b, err := os.ReadFile(path + ".sha256")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// uniqueTarget 为落盘挑一个不会毁掉已有文件的目标名。
//
// 直接 os.Rename 覆盖会让「同名但内容不同」的上一个文件无声消失——而手机端
// 通常并没有第二份备份，用户只会以为文件传丢了。规则：
//   - size 与 sha256 都与旧文件一致 → 是同一份内容的重传，复用旧文件，
//     新传的那份 .part 由调用方删除（reused=true）；
//   - 内容不同 → 另存为 "名字 (1).ext"、"名字 (2).ext"…两份都留着。
//
// 优先读 .sha256 清单而不是当场重算，避免为一个 4GB 旧文件在收尾阶段多花十几秒。
func uniqueTarget(dir, name, sum string, size int64) (path, finalName string, reused bool) {
	p := filepath.Join(dir, name)
	fi, err := os.Stat(p)
	if err != nil {
		return p, name, false // 目录里没有同名文件，正常落盘
	}
	if fi.Size() == size {
		old := sidecarSha(p)
		if old == "" {
			old = shaOf(p)
		}
		if old == sum {
			return p, name, true
		}
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; i <= 999; i++ {
		cand := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		if _, e := os.Stat(filepath.Join(dir, cand)); e != nil {
			return filepath.Join(dir, cand), cand, false
		}
	}
	cand := fmt.Sprintf("%s (%d)%s", stem, time.Now().Unix(), ext)
	return filepath.Join(dir, cand), cand, false
}

// uploadComplete 校验全部缺块已补足、算 SHA-256、重命名落盘并清位图。
// mode=overwrite 时先删除同名旧文件（含清单）再落盘，实现「覆盖」语义；
// 缺省时同名不同内容的文件自动改名共存。
func (s *Server) uploadComplete(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := safeRelPath(q.Get("name"))
	size, sErr := strconv.ParseInt(q.Get("size"), 10, 64)
	if !s.canWrite(r) {
		jsonErr(w, http.StatusForbidden, "token required")
		return
	}
	if name == "" {
		badName(w)
		return
	}
	if sErr != nil || size <= 0 {
		jsonErr(w, http.StatusBadRequest, "bad params")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	dir := s.Dir()

	bits := loadBitmap(dir, name, size)
	if missing := missingChunks(bits); len(missing) > 0 {
		jsonErrCode(w, http.StatusBadRequest, map[string]any{
			"error": "incomplete", "missing": missing, "received": contigReceived(bits, size),
		})
		return
	}

	// 覆盖模式：把同名旧文件连同清单一起清掉，下面的 uniqueTarget 就会直接用原名
	if q.Get("mode") == "overwrite" {
		old := filepath.Join(dir, name)
		if _, err := os.Stat(old); err == nil {
			if err := os.Remove(old); err != nil {
				jsonErr(w, http.StatusInternalServerError, "overwrite remove old: "+err.Error())
				return
			}
			_ = os.Remove(old + ".sha256")
		}
	}

	part := partPath(dir, name, size)
	// 规整到精确大小（末块可能与 8MiB 不对齐 / 稀疏写入造成的超长）
	if fi, err := os.Stat(part); err != nil {
		jsonErr(w, http.StatusInternalServerError, "stat part")
		return
	} else if fi.Size() != size {
		if f, e := os.OpenFile(part, os.O_RDWR, 0o644); e != nil {
			jsonErr(w, http.StatusInternalServerError, "open part")
			return
		} else {
			_ = f.Truncate(size)
			_ = f.Close()
		}
	}

	sum := shaOf(part)
	if sum == "" {
		jsonErr(w, http.StatusInternalServerError, "hash failed")
		return
	}
	// 共存落盘时目标可能在子目录里，确保目录已存在
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
		jsonErr(w, http.StatusInternalServerError, "mkdir")
		return
	}
	final, finalName, reused := uniqueTarget(dir, name, sum, size)
	if reused {
		// 内容完全一样：留旧删新，别把几 GB 的重复数据留在盘上
		_ = os.Remove(part)
		_ = os.Remove(bitsPath(dir, name, size))
	} else {
		if err := os.Rename(part, final); err != nil {
			jsonErr(w, http.StatusInternalServerError, "rename")
			return
		}
		_ = os.WriteFile(final+".sha256", []byte(sum), 0o644)
		_ = os.Remove(bitsPath(dir, name, size))
	}

	s.hub.broadcast(map[string]any{"type": "files"})
	jsonOK(w, map[string]any{"sha256": sum, "name": finalName, "reused": reused})
}

// ---------- 中断残留 ----------
// 上传中断会在接收目录留下 .part（可能是几 GB）与 .part.bits 位图。
// 这些垃圾在文件列表里不可见，必须能列出并清理。

// partialsHandler 处理 GET/DELETE /api/uploads：列出残留 / 清理残留。
// 不带 ?name= 时清理全部。
func (s *Server) partialsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		if !s.canWrite(r) {
			jsonErr(w, http.StatusForbidden, "token required")
			return
		}
		n, err := s.purgePartials(safeName(r.URL.Query().Get("name")))
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, "purge failed: "+err.Error())
			return
		}
		s.hub.broadcast(map[string]any{"type": "files"})
		jsonOK(w, map[string]any{"ok": true, "removed": n})
		return
	}
	jsonOK(w, s.scanPartials())
}

// scanPartials 列出接收目录里所有中断的上传残留。
func (s *Server) scanPartials() []map[string]any {
	return scanPartialsIn(s.Dir())
}

// scanPartialsIn 在指定目录里递归扫描中断的上传残留（按修改时间倒序）。
// 不碰 s.mu，因此已持锁的调用方（如切目录前的检查）可直接复用。
func scanPartialsIn(dir string) []map[string]any {
	out := make([]map[string]any, 0, 8)
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		n := d.Name()
		if !strings.HasSuffix(n, ".part") {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		// part 文件名：<相对路径>.<size>.part —— 去掉结尾 ".part" 再剥最后一段 size
		base := strings.TrimSuffix(rel, ".part")
		dot := strings.LastIndex(base, ".")
		if dot <= 0 { // 无 size 段（旧版残留）跳过
			return nil
		}
		size, err := strconv.ParseInt(base[dot+1:], 10, 64)
		if err != nil || size <= 0 {
			return nil
		}
		name := base[:dot]
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		have := 0
		for _, b := range loadBitmap(dir, name, size) {
			if b != 0 {
				have++
			}
		}
		out = append(out, map[string]any{
			"name": name, "size": size, "partSize": fi.Size(),
			"chunks": chunkCount(size), "have": have,
			"mtime": fi.ModTime().UnixMilli(),
		})
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		return out[i]["mtime"].(int64) > out[j]["mtime"].(int64)
	})
	return out
}

// purgePartials 删除残留及其位图；name 为空表示清理全部。返回被清理的残留个数。
func (s *Server) purgePartials(name string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.purgePartialsLocked(name)
}

// purgePartialsLocked 是 purgePartials 的无锁版本，供已持锁的调用方（如删除文件）复用，
// 避免同一个 sync.Mutex 重入导致死锁。
func (s *Server) purgePartialsLocked(name string) (int, error) {
	dir := s.Dir()
	removed := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".part") {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return nil
		}
		base := strings.TrimSuffix(filepath.ToSlash(rel), ".part")
		dot := strings.LastIndex(base, ".")
		if dot <= 0 {
			return nil
		}
		owner := base[:dot]
		if name != "" && owner != name {
			return nil
		}
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		_ = os.Remove(p + ".bits")
		removed++
		return nil
	})
	return removed, err
}
