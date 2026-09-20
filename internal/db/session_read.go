package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Reading one session back. Search says which session answers a question;
// this is how the answer itself is read. It matters most where the caller
// holds no transcripts: an archive is the durable copy of sessions whose
// .jsonl files were deleted long ago, so "go and read it on the machine it
// ran on" is often not an available answer.

// SessionDetail is one session's own columns, without its messages.
type SessionDetail struct {
	ID               string
	Project          string
	FirstQuery       string
	Tool             string
	Host             string
	WorkingDirectory string
	MessageCount     int
	StartTime        time.Time
	EndTime          time.Time
}

// SessionMessage is one row of a transcript. Role is the block type the
// adapter recorded: user, assistant, tool_use, tool_result or thinking.
type SessionMessage struct {
	ID        int64
	Role      string
	Content   string
	Timestamp time.Time
	// Agent holds the parent session's id when this row belongs to a
	// subagent transcript, and is empty otherwise.
	Agent string
}

// hasColumn reports whether a table has a column. A database written by an
// older build, or by upstream mnemo, is missing some of them, and a
// read-only caller such as the MCP server cannot migrate one.
func hasColumn(table, column string) bool {
	var n int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column,
	).Scan(&n)
	return err == nil && n > 0
}

// GetSession returns one session's own columns. The second value is false
// when no session has that id, which is a miss rather than an error.
func GetSession(id string) (SessionDetail, bool, error) {
	hostExpr := "''"
	if hasColumn("sessions", "host") {
		hostExpr = "COALESCE(host, '')"
	}

	var s SessionDetail
	var startStr, endStr string
	err := db.QueryRow(fmt.Sprintf(`
		SELECT id, project, COALESCE(first_query, ''), tool, %s,
		       COALESCE(working_directory, ''), message_count,
		       COALESCE(start_time, indexed_at, ''), COALESCE(end_time, '')
		FROM sessions WHERE id = ?
	`, hostExpr), id).Scan(&s.ID, &s.Project, &s.FirstQuery, &s.Tool, &s.Host,
		&s.WorkingDirectory, &s.MessageCount, &startStr, &endStr)
	if err == sql.ErrNoRows {
		return SessionDetail{}, false, nil
	}
	if err != nil {
		return SessionDetail{}, false, fmt.Errorf("reading session %s: %w", id, err)
	}

	s.StartTime = parseFlexibleTime(startStr)
	s.EndTime = parseFlexibleTime(endStr)
	return s, true, nil
}

// GetSessionMessages returns one page of a session's rows in the order they
// were indexed, along with how many rows match the filter in total, so a
// caller knows whether it has read the whole session.
//
// An empty roles slice means every role. Naming roles is the usual case:
// a full transcript includes tool_result rows, which hold command output and
// whole files and are far larger than the conversation they surround.
func GetSessionMessages(id string, roles []string, limit, offset int) ([]SessionMessage, int, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	where := []string{"session_id = ?"}
	args := []any{id}
	if len(roles) > 0 {
		placeholders := make([]string, len(roles))
		for i, r := range roles {
			placeholders[i] = "?"
			args = append(args, r)
		}
		where = append(where, "role IN ("+strings.Join(placeholders, ", ")+")")
	}
	clause := strings.Join(where, " AND ")

	var total int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM messages WHERE "+clause, args...,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting messages of %s: %w", id, err)
	}

	agentExpr := "''"
	if hasColumn("messages", "agent") {
		agentExpr = "COALESCE(agent, '')"
	}

	rows, err := db.Query(fmt.Sprintf(`
		SELECT id, role, content, COALESCE(timestamp, ''), %s
		FROM messages WHERE %s
		ORDER BY id
		LIMIT ? OFFSET ?
	`, agentExpr, clause), append(args, limit, offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("reading messages of %s: %w", id, err)
	}
	defer func() { _ = rows.Close() }()

	messages := make([]SessionMessage, 0, limit)
	for rows.Next() {
		var m SessionMessage
		var stamp string
		if err := rows.Scan(&m.ID, &m.Role, &m.Content, &stamp, &m.Agent); err != nil {
			// Unlike the search path, a row that cannot be read is not
			// skipped: a transcript with a hole in it reads as a complete
			// one, and nothing would say otherwise.
			return nil, 0, fmt.Errorf("reading a message of %s: %w", id, err)
		}
		m.Timestamp = parseFlexibleTime(stamp)
		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("reading messages of %s: %w", id, err)
	}

	return messages, total, nil
}
