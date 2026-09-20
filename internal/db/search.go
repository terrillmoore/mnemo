// search.go implements full-text search using SQLite FTS5 with BM25 ranking.
// Provides both message-level search (Search) and session-grouped search
// (SearchGrouped) with composite scoring: BM25 * temporal_decay * density_bonus.
package db

import (
	"fmt"
	"log"
	"math"
	"strings"
	"time"
	"unicode"
)

// SearchResult holds a single FTS5 search match with BM25 ranking.
// Snippet contains the matched text with \u27ea and \u27eb delimiters for highlighting.
type SearchResult struct {
	SessionID string
	Project   string
	Role      string
	Content   string
	Snippet   string
	Rank      float64
	Tool      string
	Model     string
	Provider  string
}

// SessionMatch holds a session-level search result with aggregated scoring.
// This is the primary return type for the redesigned search system.
type SessionMatch struct {
	SessionID    string
	Project      string
	FirstQuery   string
	MessageCount int
	Tool         string
	// Host is the machine the session was indexed on. It is empty for a row
	// written before the column existed; the machine is unknown, not local.
	Host string
	// WorkingDirectory is the directory the session ran in. It identifies
	// the work more reliably than Project, which several adapters derive
	// with a heuristic.
	WorkingDirectory string
	StartTime        time.Time
	MatchCount       int
	BestRank         float64
	FinalScore       float64
	Snippet          string
	SnippetRole      string
}

// sessionsRecordHost reports whether the sessions table has a host column.
// The fork adds it when it opens a database for writing; a database only
// ever written by upstream mnemo does not have it, and a read-only open
// cannot add one.
func sessionsRecordHost() bool {
	return hasColumn("sessions", "host")
}

// fts5MatchExpr turns what a person typed into an FTS5 MATCH expression.
//
// Each whitespace-separated term becomes a quoted string, and the terms are
// ANDed. Quoting is what makes punctuation safe: inside quotes FTS5 tokenizes
// a term instead of parsing it, so "V1.15" searches for the tokens V1 and 15
// adjacent, which is what the person meant, and no input can produce a syntax
// error.
//
// This replaces stripping a list of characters, which failed twice over. The
// list has to be right about every character SQLite's parser cares about, and
// it was not: a full stop reached FTS5 and the search died with
// `fts5: syntax error near "."`. It also threw away characters our own
// searches need, so `-j all` searched for "j all", `C++` for "C", and a
// quoted phrase could not be expressed at all.
//
// A term holding no letter or digit is dropped rather than quoted: it would
// tokenize to nothing, and an empty phrase is not a useful thing to AND.
func fts5MatchExpr(query string) (string, error) {
	var terms []string
	for _, field := range strings.Fields(query) {
		if !hasWordCharacter(field) {
			continue
		}
		terms = append(terms, `"`+strings.ReplaceAll(field, `"`, `""`)+`"`)
	}

	if len(terms) == 0 {
		return "", fmt.Errorf("search query has no word to match on (original: %q)", query)
	}

	return strings.Join(terms, " AND "), nil
}

// hasWordCharacter reports whether s holds anything FTS5 would tokenize.
func hasWordCharacter(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// Search performs a full-text search using FTS5 with BM25 ranking.
// Results include highlighted snippets with \u27ea \u27eb delimiters.
func Search(query string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 10
	}

	safeQuery, err := fts5MatchExpr(query)
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(`
		SELECT
			m.session_id,
			m.project,
			m.role,
			m.content,
			snippet(messages_fts, 0, '⟪', '⟫', '...', 256) as snippet,
			bm25(messages_fts) as rank,
			m.tool,
			m.model,
			m.provider
		FROM messages_fts
		JOIN messages m ON messages_fts.rowid = m.id
		WHERE messages_fts MATCH ?
		ORDER BY rank
		LIMIT ?
	`, safeQuery, limit)
	if err != nil {
		return nil, fmt.Errorf("search failed: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.SessionID, &r.Project, &r.Role, &r.Content, &r.Snippet, &r.Rank, &r.Tool, &r.Model, &r.Provider); err != nil {
			log.Printf("Search: rows.Scan error: %v", err)
			continue
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return results, fmt.Errorf("search iteration error: %w", err)
	}

	return results, nil
}

// SearchGrouped performs a session-level search with intelligent ranking.
// Fetches message-level FTS5 results, groups by session in Go, then
// enriches with session metadata. Ranked by BM25 * temporal_decay * density_bonus.
func SearchGrouped(query string, limit int) ([]SessionMatch, error) {
	if limit <= 0 {
		limit = 5
	}

	safeQuery, err := fts5MatchExpr(query)
	if err != nil {
		return nil, err
	}

	// Fetch message-level results with higher limit for session grouping
	fetchLimit := limit * 10
	if fetchLimit < 50 {
		fetchLimit = 50
	}

	rows, err := db.Query(`
		SELECT m.session_id, m.role,
			   snippet(messages_fts, 0, '⟪', '⟫', '...', 64) as snippet,
			   bm25(messages_fts) as rank
		FROM messages_fts
		JOIN messages m ON messages_fts.rowid = m.id
		WHERE messages_fts MATCH ?
		ORDER BY rank
		LIMIT ?
	`, safeQuery, fetchLimit)
	if err != nil {
		return nil, fmt.Errorf("grouped search failed: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// Group by session in Go
	type sessionData struct {
		matchCount      int
		bestRank        float64
		userHits        int
		bestSnippet     string
		bestSnippetRole string
		bestSnippetRank float64
		bestSnippetUser bool
	}
	sessions := make(map[string]*sessionData)
	var order []string

	for rows.Next() {
		var sessionID, role, snippet string
		var rank float64
		if err := rows.Scan(&sessionID, &role, &snippet, &rank); err != nil {
			log.Printf("SearchGrouped: rows.Scan error: %v", err)
			continue
		}

		sd, exists := sessions[sessionID]
		if !exists {
			sd = &sessionData{bestRank: rank, bestSnippetRank: 999}
			sessions[sessionID] = sd
			order = append(order, sessionID)
		}
		sd.matchCount++
		if rank < sd.bestRank {
			sd.bestRank = rank
		}
		if role == "user" {
			sd.userHits++
		}

		// Prefer user messages for snippet, then best rank
		isUser := role == "user"
		if (isUser && !sd.bestSnippetUser) || (isUser == sd.bestSnippetUser && rank < sd.bestSnippetRank) {
			sd.bestSnippet = snippet
			sd.bestSnippetRole = role
			sd.bestSnippetRank = rank
			sd.bestSnippetUser = isUser
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("grouped search iteration error: %w", err)
	}

	// A database written only by upstream mnemo has no host column, and the
	// callers that need one most, such as the MCP server, open the file
	// read-only and so cannot add it. Ask once and select accordingly.
	hostExpr := "''"
	if sessionsRecordHost() {
		hostExpr = "COALESCE(host, '')"
	}
	metaQuery := fmt.Sprintf(`
		SELECT project, COALESCE(first_query, ''), message_count, tool,
			   %s, COALESCE(working_directory, ''),
			   COALESCE(start_time, indexed_at, '')
		FROM sessions WHERE id = ?
	`, hostExpr)

	// Build SessionMatch results with session metadata
	var matches []SessionMatch
	for _, sid := range order {
		sd := sessions[sid]
		sm := SessionMatch{
			SessionID:   sid,
			MatchCount:  sd.matchCount,
			BestRank:    sd.bestRank,
			Snippet:     sd.bestSnippet,
			SnippetRole: sd.bestSnippetRole,
		}

		// Fetch session metadata (scan time as string due to mixed timestamp formats)
		var timeStr string
		err := db.QueryRow(metaQuery, sid).
			Scan(&sm.Project, &sm.FirstQuery, &sm.MessageCount, &sm.Tool, &sm.Host,
				&sm.WorkingDirectory, &timeStr)
		if err != nil {
			continue
		}
		sm.StartTime = parseFlexibleTime(timeStr)

		// Composite scoring: BM25 + density bonus + temporal decay + user-match bonus
		var temporalDecay float64
		if sm.StartTime.IsZero() {
			temporalDecay = 0.5 // Neutral fallback for sessions without timestamps
		} else {
			daysOld := time.Since(sm.StartTime).Hours() / 24
			daysOld = math.Max(0, daysOld) // Clamp: future timestamps treated as "today"
			temporalDecay = math.Exp(-0.03 * daysOld)
		}

		// Cap density bonus to prevent high-activity sessions from dominating
		densityBonus := math.Min(float64(sd.matchCount)*0.05, 1.0)
		userBonus := math.Min(float64(sd.userHits)*0.1, 1.0)

		// BM25 returns negative scores (more negative = better match).
		// Subtracting positive bonuses makes score more negative (= better rank).
		sm.FinalScore = (sd.bestRank - densityBonus - userBonus) * temporalDecay

		matches = append(matches, sm)
	}

	// Sort by FinalScore ascending (more negative = more relevant)
	for i := 1; i < len(matches); i++ {
		for j := i; j > 0 && matches[j].FinalScore < matches[j-1].FinalScore; j-- {
			matches[j], matches[j-1] = matches[j-1], matches[j]
		}
	}

	if len(matches) > limit {
		matches = matches[:limit]
	}

	return matches, nil
}

// parseFlexibleTime handles multiple timestamp formats from the database.
// All timestamps are normalized to UTC to prevent cross-timezone ranking errors.
func parseFlexibleTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	// Formats with explicit timezone info
	tzFormats := []string{
		"2006-01-02 15:04:05.999 -0700 MST",
		"2006-01-02 15:04:05.999 +0000 UTC",
		time.RFC3339,
		"2006-01-02T15:04:05Z",
	}
	for _, f := range tzFormats {
		if t, err := time.Parse(f, s); err == nil {
			return t.UTC()
		}
	}
	// SQLite datetime format (no timezone) — treat as UTC
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.UTC); err == nil {
		return t
	}
	return time.Time{}
}
