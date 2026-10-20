package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Read-only REST endpoints. They only ever touch messages.db through a dedicated read-only
// connection and never the whatsmeow session database.

const maxQueryLen = 256

// openReadOnlyDB opens a separate read-only connection to the message database.
func openReadOnlyDB(path string) (*sql.DB, error) {
	return sql.Open("sqlite3", "file:"+path+"?mode=ro&_foreign_keys=on")
}

// ChatResult is a chat as returned by GET /api/chats.
type ChatResult struct {
	JID             string     `json:"jid"`
	Name            string     `json:"name"`
	LastMessageTime *time.Time `json:"last_message_time,omitempty"`
}

// MessageResult is a message as returned by GET /api/messages and GET /api/search.
type MessageResult struct {
	ID         string     `json:"id"`
	ChatJID    string     `json:"chat_jid"`
	Sender     string     `json:"sender"`
	SenderName string     `json:"sender_name,omitempty"`
	Content    string     `json:"content"`
	Timestamp  *time.Time `json:"timestamp,omitempty"`
	IsFromMe   bool       `json:"is_from_me"`
	MediaType  string     `json:"media_type,omitempty"`
	Filename   string     `json:"filename,omitempty"`
}

// parseLimit returns def for missing/invalid values and clamps to max.
func parseLimit(raw string, def, max int) int {
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// likePattern escapes LIKE wildcards so user input matches literally (used with ESCAPE '\').
func likePattern(q string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(q) + "%"
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func onlyGET(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	return true
}

func chatsHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !onlyGET(w, r) {
			return
		}
		q := r.URL.Query()
		query := q.Get("query")
		if len(query) > maxQueryLen {
			http.Error(w, "Query too long", http.StatusBadRequest)
			return
		}
		limit := parseLimit(q.Get("limit"), 20, 100)
		pattern := likePattern(query)
		rows, err := db.Query(
			`SELECT jid, name, last_message_time FROM chats
			WHERE ? = '' OR name LIKE ? ESCAPE '\' OR jid LIKE ? ESCAPE '\'
			ORDER BY last_message_time DESC LIMIT ?`,
			query, pattern, pattern, limit,
		)
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		chats := []ChatResult{}
		for rows.Next() {
			var c ChatResult
			var name sql.NullString
			var last sql.NullTime
			if err := rows.Scan(&c.JID, &name, &last); err != nil {
				http.Error(w, "Internal error", http.StatusInternalServerError)
				return
			}
			c.Name = name.String
			if last.Valid {
				c.LastMessageTime = &last.Time
			}
			chats = append(chats, c)
		}
		if rows.Err() != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, chats)
	}
}

const messageColumns = `id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename`

func scanMessages(rows *sql.Rows, resolveSender func(string) string) ([]MessageResult, error) {
	defer rows.Close()
	msgs := []MessageResult{}
	for rows.Next() {
		var m MessageResult
		var sender, content, mediaType, filename sql.NullString
		var ts sql.NullTime
		var fromMe sql.NullBool
		if err := rows.Scan(&m.ID, &m.ChatJID, &sender, &content, &ts, &fromMe, &mediaType, &filename); err != nil {
			return nil, err
		}
		m.Sender, m.Content, m.MediaType, m.Filename = sender.String, content.String, mediaType.String, filename.String
		m.IsFromMe = fromMe.Bool
		if ts.Valid {
			m.Timestamp = &ts.Time
		}
		if resolveSender != nil && m.Sender != "" {
			m.SenderName = resolveSender(m.Sender)
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func messagesHandler(db *sql.DB, resolveSender func(string) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !onlyGET(w, r) {
			return
		}
		q := r.URL.Query()
		chatJID := q.Get("chat_jid")
		if !validChatJID(chatJID) {
			http.Error(w, "Invalid chat_jid", http.StatusBadRequest)
			return
		}
		text := q.Get("q")
		if len(text) > maxQueryLen {
			http.Error(w, "Query too long", http.StatusBadRequest)
			return
		}
		limit := parseLimit(q.Get("limit"), 20, 200)

		sqlText := `SELECT ` + messageColumns + ` FROM messages WHERE chat_jid = ?`
		args := []interface{}{chatJID}
		if raw := q.Get("before"); raw != "" {
			before, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				http.Error(w, "Invalid before (RFC3339 expected)", http.StatusBadRequest)
				return
			}
			// Timestamps are stored in local time by the sqlite driver; compare in the same zone.
			sqlText += ` AND timestamp < ?`
			args = append(args, before.In(time.Local))
		}
		if text != "" {
			sqlText += ` AND content LIKE ? ESCAPE '\'`
			args = append(args, likePattern(text))
		}
		sqlText += ` ORDER BY timestamp DESC LIMIT ?`
		args = append(args, limit)

		rows, err := db.Query(sqlText, args...)
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		msgs, err := scanMessages(rows, resolveSender)
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, msgs)
	}
}

func searchHandler(db *sql.DB, resolveSender func(string) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !onlyGET(w, r) {
			return
		}
		q := r.URL.Query()
		text := q.Get("q")
		if text == "" || len(text) > maxQueryLen {
			http.Error(w, "Parameter q is required (max 256 chars)", http.StatusBadRequest)
			return
		}
		limit := parseLimit(q.Get("limit"), 20, 100)
		rows, err := db.Query(
			`SELECT `+messageColumns+` FROM messages WHERE content LIKE ? ESCAPE '\' ORDER BY timestamp DESC LIMIT ?`,
			likePattern(text), limit,
		)
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		msgs, err := scanMessages(rows, resolveSender)
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, msgs)
	}
}
