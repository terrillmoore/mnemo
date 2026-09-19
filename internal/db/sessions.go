// sessions.go handles session-level CRUD operations. A session aggregates all
// messages from a single AI coding conversation. Provides both direct and
// transactional (Tx) variants for atomic indexer operations.
package db

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

// hostName is the machine every session inserted by this process is attributed
// to. It defaults to the system hostname, which is right for a workstation
// indexing its own history. An archive box indexing another machine's
// transcripts must override it with SetHost, or every imported session will be
// labelled with the archive's own name.
var (
	hostMu   sync.RWMutex
	hostName = defaultHost()
)

func defaultHost() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// SetHost sets the host recorded on sessions inserted from now on. Call it
// before InitDB: the first-run migration claims pre-host-column sessions for
// whatever host is set at that moment.
func SetHost(name string) {
	hostMu.Lock()
	defer hostMu.Unlock()
	hostName = name
}

// Host reports the host that sessions are currently attributed to.
func Host() string {
	hostMu.RLock()
	defer hostMu.RUnlock()
	return hostName
}

// ClaimEmptyHosts attributes every session with no host to the named one and
// reports how many it changed. An empty host means the row was written before
// the column existed, or by an upstream build that does not know about it;
// either way the row's origin was never recorded, so claiming it is a
// statement about where the database has lived, not a correction.
func ClaimEmptyHosts(name string) (int64, error) {
	res, err := db.Exec("UPDATE sessions SET host = ? WHERE host = '' OR host IS NULL", name)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetAllHosts attributes every session to the named host, overwriting values
// already there, and reports how many it changed.
func SetAllHosts(name string) (int64, error) {
	res, err := db.Exec("UPDATE sessions SET host = ?", name)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// HostCounts reports how many sessions each host holds, commonest first. An
// empty host name means the sessions have not been claimed.
func HostCounts() (map[string]int, error) {
	rows, err := db.Query("SELECT host, COUNT(*) FROM sessions GROUP BY host ORDER BY 2 DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var h string
		var n int
		if err := rows.Scan(&h, &n); err != nil {
			return nil, err
		}
		counts[h] = n
	}
	return counts, rows.Err()
}

// Session aggregates all messages from a single AI coding conversation.
// The ID is typically derived from the source tool's session identifier.
type Session struct {
	ID                   string
	Project              string
	FirstQuery           string
	MessageCount         int
	Tool                 string
	FilePath             string
	IndexedAt            time.Time
	Model                string
	Provider             string
	TotalInputTokens     int
	TotalOutputTokens    int
	TotalCacheRead       int
	TotalCacheWrite      int
	TotalReasoningTokens int
	TotalCostUSD         float64
	CLIVersion           string
	GitBranch            string
	WorkingDirectory     string
	StartTime            time.Time
	EndTime              time.Time
	Agent                string
	Date                 string
	// Host is the machine the session happened on. Left empty, the package
	// default from SetHost is used.
	Host string
}

func insertSession(ex execer, sess Session) error {
	host := sess.Host
	if host == "" {
		host = Host()
	}
	_, err := ex.Exec(`
		INSERT OR REPLACE INTO sessions (
			id, project, first_query, message_count, tool, file_path, indexed_at,
			model, provider, total_input_tokens, total_output_tokens,
			total_cache_read, total_cache_write, total_reasoning_tokens, total_cost_usd,
			cli_version, git_branch, working_directory, start_time, end_time, agent, date,
			host
		) VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		sess.ID, sess.Project, sess.FirstQuery, sess.MessageCount, sess.Tool, sess.FilePath,
		sess.Model, sess.Provider, sess.TotalInputTokens, sess.TotalOutputTokens,
		sess.TotalCacheRead, sess.TotalCacheWrite, sess.TotalReasoningTokens, sess.TotalCostUSD,
		sess.CLIVersion, sess.GitBranch, sess.WorkingDirectory, sess.StartTime, sess.EndTime,
		sess.Agent, sess.Date, host,
	)
	return err
}

// InsertSession upserts a session record with full metadata using the global DB connection.
func InsertSession(sess Session) error {
	return insertSession(db, sess)
}

// TxInsertSession inserts a session record within a transaction.
func TxInsertSession(tx *sql.Tx, sess Session) error {
	return insertSession(tx, sess)
}

// GetIndexedSessions returns a map of session_id -> indexed_at for incremental indexing.
// Callers compare file mtime against indexed_at to skip unchanged sessions.
func GetIndexedSessions() (map[string]time.Time, error) {
	result := make(map[string]time.Time)
	rows, err := db.Query("SELECT id, indexed_at FROM sessions")
	if err != nil {
		return result, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var id string
		var indexedAt time.Time
		if err := rows.Scan(&id, &indexedAt); err != nil {
			log.Printf("GetIndexedSessions: rows.Scan error: %v", err)
			continue
		}
		result[id] = indexedAt
	}
	if err := rows.Err(); err != nil {
		return result, fmt.Errorf("GetIndexedSessions iteration error: %w", err)
	}
	return result, nil
}

func insertSessionSimple(ex execer, id, project, firstQuery, filePath, tool string, msgCount int) error {
	_, err := ex.Exec(`
		INSERT OR REPLACE INTO sessions (id, project, first_query, message_count, tool, file_path, indexed_at, host)
		VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, ?)
	`, id, project, firstQuery, msgCount, tool, filePath, Host())
	return err
}

// InsertSessionSimple upserts a session record with minimal fields using the global DB connection.
func InsertSessionSimple(id, project, firstQuery, filePath, tool string, msgCount int) error {
	return insertSessionSimple(db, id, project, firstQuery, filePath, tool, msgCount)
}

// TxInsertSessionSimple inserts a session record (minimal fields) within a transaction.
func TxInsertSessionSimple(tx *sql.Tx, id, project, firstQuery, filePath, tool string, msgCount int) error {
	return insertSessionSimple(tx, id, project, firstQuery, filePath, tool, msgCount)
}

// UpdateSessionTokens increments a session's aggregate token counts and cost.
func UpdateSessionTokens(sessionID string, inputTokens, outputTokens, cacheRead, cacheWrite int, costUSD float64, model, provider string) error {
	_, err := db.Exec(`
		UPDATE sessions SET
			total_input_tokens = total_input_tokens + ?,
			total_output_tokens = total_output_tokens + ?,
			total_cache_read = total_cache_read + ?,
			total_cache_write = total_cache_write + ?,
			total_cost_usd = total_cost_usd + ?,
			model = COALESCE(NULLIF(?, ''), model),
			provider = COALESCE(NULLIF(?, ''), provider)
		WHERE id = ?
	`, inputTokens, outputTokens, cacheRead, cacheWrite, costUSD, model, provider, sessionID)
	return err
}

// RecentSession holds a session returned by GetRecentSessions.
type RecentSession struct {
	ID           string
	Project      string
	FirstQuery   string
	MessageCount int
	Tool         string
	IndexedAt    time.Time
	Model        string
	Provider     string
	InputTokens  int
	OutputTokens int
	CostUSD      float64
	// Host is the machine the session was indexed on, empty when the row
	// records none. StartTime falls back to indexed_at when the adapter
	// recorded no start.
	Host             string
	WorkingDirectory string
	StartTime        time.Time
}

// GetRecentSessions returns the most recent sessions ordered by indexed_at descending.
func GetRecentSessions(limit int) ([]RecentSession, error) {
	if limit <= 0 {
		limit = 10
	}

	hostExpr := "''"
	if sessionsRecordHost() {
		hostExpr = "COALESCE(host, '')"
	}
	rows, err := db.Query(fmt.Sprintf(`
		SELECT id, project, first_query, message_count, tool, indexed_at,
		       model, provider, total_input_tokens, total_output_tokens, total_cost_usd,
		       %s, COALESCE(working_directory, ''), COALESCE(start_time, indexed_at, '')
		FROM sessions
		ORDER BY indexed_at DESC
		LIMIT ?
	`, hostExpr), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var sessions []RecentSession
	for rows.Next() {
		var s RecentSession
		var startStr string
		err := rows.Scan(&s.ID, &s.Project, &s.FirstQuery, &s.MessageCount, &s.Tool, &s.IndexedAt,
			&s.Model, &s.Provider, &s.InputTokens, &s.OutputTokens, &s.CostUSD,
			&s.Host, &s.WorkingDirectory, &startStr)
		if err != nil {
			log.Printf("GetRecentSessions: rows.Scan error: %v", err)
			continue
		}
		s.StartTime = parseFlexibleTime(startStr)
		sessions = append(sessions, s)
	}
	if err := rows.Err(); err != nil {
		return sessions, fmt.Errorf("GetRecentSessions iteration error: %w", err)
	}

	return sessions, nil
}

// GetMaxIndexedAtByTool returns the max(indexed_at) per tool for incremental DB-level checks.
func GetMaxIndexedAtByTool() (map[string]time.Time, error) {
	result := make(map[string]time.Time)
	rows, err := db.Query("SELECT tool, MAX(indexed_at) FROM sessions GROUP BY tool")
	if err != nil {
		return result, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var tool, maxIndexedAtStr string
		if err := rows.Scan(&tool, &maxIndexedAtStr); err != nil {
			log.Printf("GetMaxIndexedAtByTool: rows.Scan error: %v", err)
			continue
		}
		// Parse the SQLite CURRENT_TIMESTAMP format
		if t, err := time.Parse("2006-01-02 15:04:05", maxIndexedAtStr); err == nil {
			result[tool] = t
		} else if t, err := time.Parse(time.RFC3339, maxIndexedAtStr); err == nil {
			result[tool] = t
		}
	}
	if err := rows.Err(); err != nil {
		return result, fmt.Errorf("GetMaxIndexedAtByTool iteration error: %w", err)
	}
	return result, nil
}

// GetStats returns the total number of sessions and messages in the database.
func GetStats() (int, int, error) {
	var sessionCount, messageCount int

	err := db.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&sessionCount)
	if err != nil {
		return 0, 0, err
	}

	err = db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&messageCount)
	if err != nil {
		return 0, 0, err
	}

	return sessionCount, messageCount, nil
}
