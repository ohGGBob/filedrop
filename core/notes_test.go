package core

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newNoteTest 返回一个只服务便签的实例：便签文件指向临时目录，令牌鉴权开启。
// 用 httptest.NewRequest 直接打处理函数——它默认把 RemoteAddr 设成 192.0.2.1，
// 不是回环地址，因此正好能压到「没令牌就 403」这条分支（回环豁免会掩盖它）。
func newNoteTest(t *testing.T) *Server {
	t.Helper()
	notesTestPath = filepath.Join(t.TempDir(), "filedrop-notes.json")
	t.Cleanup(func() { notesTestPath = "" })
	return New(0, t.TempDir(), false)
}

func postNote(s *Server, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/notes?t="+s.Token(), strings.NewReader(body))
	w := httptest.NewRecorder()
	s.notesHandler(w, r)
	return w
}

func listNotesViaHandler(t *testing.T, s *Server) []Note {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/notes?t="+s.Token(), nil)
	w := httptest.NewRecorder()
	s.notesHandler(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("列便签失败 %d: %s", w.Code, w.Body.String())
	}
	var list []Note
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("解析便签列表失败: %v / %s", err, w.Body.String())
	}
	return list
}

func TestNotesRequireToken(t *testing.T) {
	s := newNoteTest(t)

	noToken := httptest.NewRequest(http.MethodPost, "/api/notes", strings.NewReader("密码是 1234"))
	w := httptest.NewRecorder()
	s.notesHandler(w, noToken)
	if w.Code != http.StatusForbidden {
		t.Errorf("无令牌 POST /api/notes 返回 %d，期望 403", w.Code)
	}
	read := httptest.NewRequest(http.MethodGet, "/api/notes", nil)
	w = httptest.NewRecorder()
	s.notesHandler(w, read)
	if w.Code != http.StatusForbidden {
		t.Errorf("无令牌 GET /api/notes 返回 %d，期望 403（便签不落盘不等于可以公开读）", w.Code)
	}
	if got := listNotesViaHandler(t, s); len(got) != 0 {
		t.Errorf("被拒的写入竟然落盘了: %+v", got)
	}
}

func TestNotesAddListDelete(t *testing.T) {
	s := newNoteTest(t)

	first := postNote(s, "  wifi 密码：abc-123\n第二行保留  ")
	if first.Code != http.StatusOK {
		t.Fatalf("新增失败 %d: %s", first.Code, first.Body.String())
	}
	var n1 Note
	if err := json.Unmarshal(first.Body.Bytes(), &n1); err != nil {
		t.Fatal(err)
	}
	if n1.Text != "wifi 密码：abc-123\n第二行保留" {
		t.Errorf("首尾空白未清或内部换行被吃掉: %q", n1.Text)
	}
	if n1.Size != len(n1.Text) || n1.ID == "" {
		t.Errorf("id/size 不对: %+v", n1)
	}
	postNote(s, "ssh root@192.168.1.10")

	list := listNotesViaHandler(t, s)
	if len(list) != 2 {
		t.Fatalf("期望 2 条，实得 %d: %+v", len(list), list)
	}
	if list[0].Text != "ssh root@192.168.1.10" {
		t.Error("新收到的没排在最前")
	}

	del := httptest.NewRequest(http.MethodDelete, "/api/notes?t="+s.Token()+"&id="+list[0].ID, nil)
	w := httptest.NewRecorder()
	s.notesHandler(w, del)
	if w.Code != http.StatusOK {
		t.Fatalf("删除失败 %d: %s", w.Code, w.Body.String())
	}
	if got := listNotesViaHandler(t, s); len(got) != 1 || got[0].ID != n1.ID {
		t.Errorf("删除后列表不符: %+v", got)
	}

	missing := httptest.NewRequest(http.MethodDelete, "/api/notes?t="+s.Token()+"&id=deadbeef", nil)
	w = httptest.NewRecorder()
	s.notesHandler(w, missing)
	if w.Code != http.StatusNotFound {
		t.Errorf("删不存在的 id 返回 %d，期望 404", w.Code)
	}
}

// TestNotesRejectBadInput 三类该拒的一律 400：超长、空、非 UTF-8。
func TestNotesRejectBadInput(t *testing.T) {
	s := newNoteTest(t)

	if w := postNote(s, strings.Repeat("啊", 100*1024)); w.Code != http.StatusBadRequest {
		t.Errorf("300KB 文本返回 %d，期望 400（每条 3 字节）", w.Code)
	}
	if w := postNote(s, "   \n\t  "); w.Code != http.StatusBadRequest {
		t.Errorf("纯空白返回 %d，期望 400", w.Code)
	}
	if w := postNote(s, "\xff\xfe不是utf8"); w.Code != http.StatusBadRequest {
		t.Errorf("非法编码返回 %d，期望 400", w.Code)
	}
	if got := listNotesViaHandler(t, s); len(got) != 0 {
		t.Errorf("被拒的内容进了存储: %+v", got)
	}
}

func TestTrimNotes(t *testing.T) {
	list := make([]Note, 0, maxNoteCount+50)
	for i := 0; i < maxNoteCount+50; i++ {
		list = append(list, Note{ID: string(rune('a' + i%26)), Text: "x", At: int64(i), Size: 1})
	}
	if got := trimNotes(list); len(got) != maxNoteCount || got[0].At != int64(len(list)-1) {
		t.Errorf("条数上限未生效: %d 条，首条 at=%d", len(got), got[0].At)
	}

	big := []Note{
		{ID: "old", At: 1, Size: maxNoteTotal},
		{ID: "new", At: 2, Size: 10},
	}
	got := trimNotes(big)
	if len(got) != 1 || got[0].ID != "new" {
		t.Errorf("总字节上限未按新→旧保留: %+v", got)
	}
}

// TestNotesSurviveGarbage 便签文件被写坏（断电 / 杀进程留下的半截 JSON）时，
// 接口不能 500，也不能一直读不出来；最差结果只是丢掉历史便签。
func TestNotesSurviveGarbage(t *testing.T) {
	s := newNoteTest(t)
	if err := os.WriteFile(notesPath(), []byte(`[{"id":"a","text":"写到一半`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := listNotesViaHandler(t, s); len(got) != 0 {
		t.Errorf("坏文件竟然读出了内容: %+v", got)
	}
	if w := postNote(s, "重新写一条"); w.Code != http.StatusOK {
		t.Fatalf("坏文件后新增失败: %d %s", w.Code, w.Body.String())
	}
	got := listNotesViaHandler(t, s)
	if len(got) != 1 || got[0].Text != "重新写一条" {
		t.Errorf("覆盖保存后列表不符: %+v", got)
	}
}
