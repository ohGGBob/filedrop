package core

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// 文本快传：手机粘一段验证码 / 命令 / 地址过来，电脑端一键复制走。
// 走文件那条路太亏——几十字节也要「选文件 → 分块 → 落盘 → 再下载」。

const (
	maxNoteBytes = 256 * 1024 // 单条上限
	maxNoteCount = 200        // 总条数上限
	maxNoteTotal = 8 * 1024 * 1024
	notesFile    = "filedrop-notes.json"
)

// Note 是一条跨设备传递的文字片段。
type Note struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	At   int64  `json:"at"` // Unix 毫秒
	Size int    `json:"size"`
}

// notesTestPath 仅由测试设置，把便签文件指到临时目录去。
// 不这么做的话，生产路径固定在 exe 同级，测试之间会互相看到对方写的便签。
var notesTestPath string

func notesPath() string {
	if notesTestPath != "" {
		return notesTestPath
	}
	return dataPath(notesFile)
}

// loadNotes 读便签文件。文件不存在等同于空，格式坏了也当空处理：
// 一截写坏的 JSON 不该让整个服务起不来，最坏是丢历史便签。
func loadNotes() ([]Note, error) {
	b, err := os.ReadFile(notesPath())
	if err != nil {
		if os.IsNotExist(err) {
			return []Note{}, nil
		}
		return nil, err
	}
	var list []Note
	if err := json.Unmarshal(b, &list); err != nil {
		return []Note{}, nil
	}
	if list == nil {
		list = []Note{}
	}
	return list, nil
}

// saveNotes 先写临时文件再改名替换：便签是唯一有共享状态的文件，
// 中途断电或被杀进程不能留下半截 JSON。
func saveNotes(list []Note) error {
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := notesPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, notesPath())
}

// trimNotes 按时间倒序裁到条数与总字节上限。
func trimNotes(list []Note) []Note {
	sort.SliceStable(list, func(i, j int) bool { return list[i].At > list[j].At })
	total := 0
	out := list[:0]
	for _, n := range list {
		if len(out) >= maxNoteCount || total+n.Size > maxNoteTotal {
			break
		}
		total += n.Size
		out = append(out, n)
	}
	return out
}

// notesHandler 路由到 /api/notes。三个操作一律要令牌（本机回环豁免）：
// 便签内容常是验证码、密码、内网地址，不像文件那样可以对着局域网敞开读。
func (s *Server) notesHandler(w http.ResponseWriter, r *http.Request) {
	if !s.canWrite(r) {
		jsonErr(w, http.StatusForbidden, "token required")
		return
	}
	switch r.Method {
	case http.MethodPost:
		s.addNote(w, r)
	case http.MethodDelete:
		s.deleteNote(w, r)
	default:
		s.listNotes(w)
	}
}

func (s *Server) listNotes(w http.ResponseWriter) {
	s.notesMu.Lock()
	defer s.notesMu.Unlock()
	list, err := loadNotes()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "read notes")
		return
	}
	jsonOK(w, list)
}

func (s *Server) addNote(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxNoteBytes+1))
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "read body")
		return
	}
	if len(raw) > maxNoteBytes {
		jsonErr(w, http.StatusBadRequest, "单条文字上限 256 KB")
		return
	}
	if !utf8.Valid(raw) {
		jsonErr(w, http.StatusBadRequest, "只接受 UTF-8 文本")
		return
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		jsonErr(w, http.StatusBadRequest, "内容为空")
		return
	}
	n := Note{Text: text, At: time.Now().UnixMilli(), Size: len(text)}

	s.notesMu.Lock()
	list, err := loadNotes()
	if err == nil {
		n.ID = newNoteID(list)
		list = trimNotes(append([]Note{n}, list...))
		err = saveNotes(list)
	}
	s.notesMu.Unlock()

	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "save notes")
		return
	}
	s.hub.broadcast(map[string]any{"type": "notes", "id": n.ID})
	jsonOK(w, n)
}

func (s *Server) deleteNote(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		jsonErr(w, http.StatusBadRequest, "bad params")
		return
	}
	s.notesMu.Lock()
	list, err := loadNotes()
	var kept []Note
	found := false
	for _, n := range list {
		if n.ID == id {
			found = true
			continue
		}
		kept = append(kept, n)
	}
	if found {
		err = saveNotes(kept)
	}
	s.notesMu.Unlock()

	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "save notes")
		return
	}
	if !found {
		jsonErr(w, http.StatusNotFound, "note not found")
		return
	}
	s.hub.broadcast(map[string]any{"type": "notes"})
	jsonOK(w, map[string]any{"ok": true})
}

// newNoteID 在现存便签之外取一个 8 位十六进制 id。
// 收 list 而不是自己加锁重读：调用方（addNote）已经持着 notesMu 并读过一次文件了。
func newNoteID(list []Note) string {
	taken := make(map[string]bool, len(list))
	for _, n := range list {
		taken[n.ID] = true
	}
	for i := 0; i < 20; i++ {
		if id := randHex(4); !taken[id] {
			return id
		}
	}
	return randHex(8)
}
