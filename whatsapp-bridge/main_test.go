package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSafeFilename(t *testing.T) {
	cases := map[string]string{
		"../x":           "x",
		"..\\x":          "x",
		"/abs/path.txt":  "path.txt",
		"../whatsapp.db": "whatsapp.db",
		"":               "",
		".":              "",
		"..":             "",
		"/":              "",
		"a/..":           "",
		"report.pdf":     "report.pdf",
	}
	for in, want := range cases {
		if got := safeFilename(in); got != want {
			t.Errorf("safeFilename(%q) = %q, want %q", in, got, want)
		}
	}
	if got := fallbackFilename("../AB/c"); got != "media____AB_c" {
		t.Errorf("fallbackFilename = %q", got)
	}
}

func TestValidChatJID(t *testing.T) {
	cases := map[string]bool{
		"123456789@s.whatsapp.net":    true,
		"12036302@g.us":               true,
		"1234:5@s.whatsapp.net":       true,
		"..@s.whatsapp.net":           false,
		"a/b@s.whatsapp.net":          false,
		"../../etc":                   false,
		"":                            false,
		"x@s.whatsapp.net' OR '1'='1": false,
	}
	for in, want := range cases {
		if got := validChatJID(in); got != want {
			t.Errorf("validChatJID(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestStoreMediaPath(t *testing.T) {
	dir, p, err := storeMediaPath("store", "1234:5@s.whatsapp.net", "a.jpg")
	if err != nil || dir != filepath.Join("store", "1234_5@s.whatsapp.net") || p != filepath.Join(dir, "a.jpg") {
		t.Fatalf("valid case: %q %q %v", dir, p, err)
	}
	bad := [][2]string{
		{".", "whatsapp.db"},                   // would land in store/ root
		{"..", "x"},                            // escapes store/
		{"x@s.whatsapp.net", "../whatsapp.db"}, // unsanitized filename
		{"x@s.whatsapp.net", ".."},
		{"a/b", "x"},
	}
	for _, c := range bad {
		if _, _, err := storeMediaPath("store", c[0], c[1]); err == nil {
			t.Errorf("storeMediaPath(%q, %q) accepted", c[0], c[1])
		}
	}
}

func TestResolveMediaPath(t *testing.T) {
	root := t.TempDir()
	media := filepath.Join(root, "media")
	store := filepath.Join(media, "store") // store nested in media must still be refused
	outside := filepath.Join(root, "outside")
	for _, d := range []string{media, store, outside} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p string) {
		if err := os.WriteFile(p, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	inside := filepath.Join(media, "ok.jpg")
	write(inside)
	secret := filepath.Join(outside, "secret.txt")
	write(secret)
	write(filepath.Join(store, "whatsapp.db"))
	link := filepath.Join(media, "link.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}

	// Legacy behaviour when WA_MEDIA_DIR is unset
	if got, err := resolveMediaPath(secret, "", store); err != nil || got != secret {
		t.Errorf("legacy: %q %v", got, err)
	}
	if _, err := resolveMediaPath(inside, media, store); err != nil {
		t.Errorf("inside rejected: %v", err)
	}
	for name, p := range map[string]string{
		"outside":   secret,
		"traversal": filepath.Join(media, "..", "outside", "secret.txt"),
		"symlink":   link,
		"store":     filepath.Join(store, "whatsapp.db"),
		"dir":       media,
		"missing":   filepath.Join(media, "nope"),
	} {
		if _, err := resolveMediaPath(p, media, store); err == nil {
			t.Errorf("%s accepted: %s", name, p)
		}
	}
}

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "messages.db")
	rw, err := sql.Open("sqlite3", "file:"+path+"?_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	defer rw.Close()
	_, err = rw.Exec(`
		CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP);
		CREATE TABLE messages (
			id TEXT, chat_jid TEXT, sender TEXT, content TEXT, timestamp TIMESTAMP, is_from_me BOOLEAN,
			media_type TEXT, filename TEXT, url TEXT, media_key BLOB, file_sha256 BLOB,
			file_enc_sha256 BLOB, file_length INTEGER,
			PRIMARY KEY (id, chat_jid), FOREIGN KEY (chat_jid) REFERENCES chats(jid));`)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local)
	jid := "111@s.whatsapp.net"
	if _, err := rw.Exec(`INSERT INTO chats VALUES (?, ?, ?), (?, ?, ?)`,
		jid, "Alice", base, "222@g.us", "Group_X", base.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	contents := []string{"50% off", "500 off", "hello", "under_score", "underXscore"}
	for i := 0; i < 250; i++ {
		c := "filler"
		if i < len(contents) {
			c = contents[i]
		}
		if _, err := rw.Exec(`INSERT INTO messages VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			"m"+itoa(i), jid, "999", c, base.Add(-time.Duration(i)*time.Minute), i%2 == 0,
			"", "", "https://secret", []byte("key"), nil, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	ro, err := openReadOnlyDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ro.Close() })
	return ro
}

func itoa(i int) string {
	return string(rune('a'+i/26/26%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i%26))
}

func get(t *testing.T, h http.HandlerFunc, method, url string, out interface{}) int {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(method, url, nil))
	if out != nil && rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("decode %s: %v (%s)", url, err, rec.Body.String())
		}
	}
	return rec.Code
}

func TestReadHandlers(t *testing.T) {
	db := newTestDB(t)
	msgs := messagesHandler(db, func(s string) string { return "+" + s })
	search := searchHandler(db, nil)
	chats := chatsHandler(db)

	var ms []MessageResult
	if code := get(t, msgs, "GET", "/api/messages?chat_jid=111@s.whatsapp.net&limit=1000", &ms); code != 200 || len(ms) != 200 {
		t.Fatalf("limit clamp: code=%d len=%d", code, len(ms))
	}
	if ms[0].ID != "maaa" || ms[0].SenderName != "+999" || !ms[0].IsFromMe {
		t.Errorf("first message: %+v", ms[0])
	}
	if code := get(t, msgs, "GET", "/api/messages?chat_jid=111@s.whatsapp.net", &ms); code != 200 || len(ms) != 20 {
		t.Errorf("default limit: %d %d", code, len(ms))
	}
	before := time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local).Add(-2 * time.Minute).Format(time.RFC3339)
	if code := get(t, msgs, "GET", "/api/messages?chat_jid=111@s.whatsapp.net&limit=1&before="+before, &ms); code != 200 || len(ms) != 1 || ms[0].ID != "maad" {
		t.Errorf("before: %d %+v", code, ms)
	}

	// '%' and '_' must match literally
	if code := get(t, search, "GET", "/api/search?q=50%25", &ms); code != 200 || len(ms) != 1 || ms[0].Content != "50% off" {
		t.Errorf("literal %%: %d %+v", code, ms)
	}
	if code := get(t, msgs, "GET", "/api/messages?chat_jid=111@s.whatsapp.net&q=under_", &ms); code != 200 || len(ms) != 1 || ms[0].Content != "under_score" {
		t.Errorf("literal _: %d %+v", code, ms)
	}
	if code := get(t, search, "GET", "/api/search?q=filler&limit=500", &ms); code != 200 || len(ms) != 100 {
		t.Errorf("search clamp: %d %d", code, len(ms))
	}

	var cs []ChatResult
	if code := get(t, chats, "GET", "/api/chats?query=Group_", &cs); code != 200 || len(cs) != 1 || cs[0].Name != "Group_X" {
		t.Errorf("chats query: %d %+v", code, cs)
	}
	if code := get(t, chats, "GET", "/api/chats?limit=999", &cs); code != 200 || len(cs) != 2 || cs[0].JID != "111@s.whatsapp.net" {
		t.Errorf("chats all: %d %+v", code, cs)
	}

	// Raw JSON must not expose url or media_key
	rec := httptest.NewRecorder()
	msgs(rec, httptest.NewRequest("GET", "/api/messages?chat_jid=111@s.whatsapp.net&limit=1", nil))
	var raw []map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &raw)
	if _, ok := raw[0]["url"]; ok {
		t.Error("url leaked")
	}
	if _, ok := raw[0]["media_key"]; ok {
		t.Error("media_key leaked")
	}

	for _, h := range []http.HandlerFunc{msgs, search, chats} {
		if code := get(t, h, "POST", "/api/x?chat_jid=111@s.whatsapp.net&q=a", nil); code != http.StatusMethodNotAllowed {
			t.Errorf("POST: %d", code)
		}
	}
	for _, u := range []string{"/api/messages?chat_jid=..%2Fx", "/api/messages?chat_jid=a/b", "/api/messages", "/api/messages?chat_jid=111@s.whatsapp.net&before=yesterday"} {
		if code := get(t, msgs, "GET", u, nil); code != http.StatusBadRequest {
			t.Errorf("%s: %d", u, code)
		}
	}
	if code := get(t, search, "GET", "/api/search", nil); code != http.StatusBadRequest {
		t.Errorf("empty search: %d", code)
	}

	// The connection is read-only
	if _, err := db.Exec(`DELETE FROM messages`); err == nil {
		t.Error("read-only connection accepted a write")
	}
}
