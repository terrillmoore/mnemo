// search.go implements full-text search using SQLite FTS5 with BM25 ranking.
// Provides both message-level search (Search) and session-grouped search
// (SearchGrouped) with composite scoring: BM25 * temporal_decay * density_bonus.
package db

import (
	"database/sql"
	"fmt"
	"log"
	"math"
	"sort"
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
	return fts5MatchExprMode(query, MatchAll)
}

// MatchMode says whether every term has to appear in one message or any of
// them will do.
type MatchMode string

const (
	MatchAll MatchMode = "all"
	MatchAny MatchMode = "any"
)

// fts5MatchExprMode builds the MATCH expression for one mode.
func fts5MatchExprMode(query string, mode MatchMode) (string, error) {
	terms := fts5Terms(query)
	if len(terms) == 0 {
		return "", fmt.Errorf("search query has no word to match on (original: %q)", query)
	}

	joiner := " AND "
	if mode == MatchAny {
		joiner = " OR "
	}
	return strings.Join(terms, joiner), nil
}

// fts5Terms quotes each term of a query, dropping any FTS5 would tokenize to
// nothing.
func fts5Terms(query string) []string {
	var terms []string
	for _, field := range strings.Fields(query) {
		if !hasWordCharacter(field) {
			continue
		}
		terms = append(terms, `"`+strings.ReplaceAll(field, `"`, `""`)+`"`)
	}
	return terms
}

// termsWithoutMatches reports which of a query's terms appear nowhere in the
// index, spelled as the caller typed them. A caller whose nine-word sentence
// found nothing can then drop the two words that were never going to match
// instead of guessing which.
func termsWithoutMatches(query string) []string {
	var missing []string
	fields := strings.Fields(query)
	kept := make([]string, 0, len(fields))
	for _, f := range fields {
		if hasWordCharacter(f) {
			kept = append(kept, f)
		}
	}

	for i, term := range fts5Terms(query) {
		var one int
		err := db.QueryRow(
			`SELECT 1 FROM messages_fts WHERE messages_fts MATCH ? LIMIT 1`, term,
		).Scan(&one)
		if err == sql.ErrNoRows {
			missing = append(missing, kept[i])
		}
	}
	return missing
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

// SearchFilter narrows a search to part of the corpus. An empty field does
// not filter.
//
// Host and WorkingDirectory are the dependable way to say "the work done
// there": Project is derived by each adapter with a heuristic, so a WSL
// session keeps two path segments while a Windows one keeps a mangled full
// path, and neither is predictable from the outside.
type SearchFilter struct {
	// Host matches the machine exactly, as `mnemo migrate host` lists them.
	Host string
	// WorkingDirectory matches any session whose directory contains this
	// text, so a caller can pass a fragment of a long Windows path.
	WorkingDirectory string
	// Project matches any session whose derived project name contains this
	// text, ignoring case. It used to be an exact match applied after the
	// query, which turned a near miss into an empty result with no hint
	// that the filter had done it.
	Project string
	// Roles limits which block types count as a hit: user, assistant,
	// tool_use, tool_result or thinking. A tool_use hit is a command
	// someone ran, not a conclusion anyone reached.
	Roles []string
	// Since keeps sessions that started on or after this date, written
	// YYYY-MM-DD. The stored timestamps begin with that same fixed-width
	// date, so a text comparison orders them correctly this far.
	Since string
}

// sqlWhere renders the filter as SQL over the joined sessions row s and
// message row m, and the values to bind.
func (f SearchFilter) sqlWhere() (string, []any) {
	var clauses []string
	var args []any

	if f.Host != "" && hasColumn("sessions", "host") {
		clauses = append(clauses, "s.host = ?")
		args = append(args, f.Host)
	}
	if f.WorkingDirectory != "" {
		clauses = append(clauses, "COALESCE(s.working_directory, '') LIKE ?")
		args = append(args, "%"+f.WorkingDirectory+"%")
	}
	if f.Project != "" {
		clauses = append(clauses, "LOWER(s.project) LIKE LOWER(?)")
		args = append(args, "%"+f.Project+"%")
	}
	if len(f.Roles) > 0 {
		placeholders := make([]string, len(f.Roles))
		for i, r := range f.Roles {
			placeholders[i] = "?"
			args = append(args, r)
		}
		clauses = append(clauses, "m.role IN ("+strings.Join(placeholders, ", ")+")")
	}
	if f.Since != "" {
		clauses = append(clauses, "COALESCE(s.start_time, s.indexed_at, '') >= ?")
		args = append(args, f.Since)
	}

	if len(clauses) == 0 {
		return "", nil
	}
	return " AND " + strings.Join(clauses, " AND "), args
}

// GroupedSearch is what a session-level search found and how it found it.
type GroupedSearch struct {
	Matches []SessionMatch
	// Mode is the match that produced the results: "all" when every term had
	// to appear in one message, "any" when that found nothing and the search
	// fell back to matching some of them.
	Mode MatchMode
	// TermsWithoutMatches lists terms that appear nowhere in the index.
	// Filled in only when the strict pass found nothing, since that is when
	// a caller needs to know which word to drop.
	TermsWithoutMatches []string
	// SkippedSessions counts sessions that matched but whose own row could
	// not be read, so a caller is told the results are short rather than
	// being handed a quietly truncated answer.
	SkippedSessions int
}

// SearchGroupedExplained runs the session-level search and says how it
// answered. A question written as a sentence asks for every word in one
// message, which is the one thing that reliably finds nothing, so a strict
// pass that comes back empty is retried as "any term". Which pass produced
// the results is reported rather than left for the caller to assume.
func SearchGroupedExplained(query string, limit int, filter SearchFilter) (GroupedSearch, error) {
	return SearchGroupedWithSnippet(query, limit, filter, 0)
}

// SearchGroupedWithSnippet is SearchGroupedExplained with the snippet width
// named. snippetTokens of 0 takes the default.
func SearchGroupedWithSnippet(query string, limit int, filter SearchFilter, snippetTokens int) (GroupedSearch, error) {
	matches, skipped, err := searchGroupedSnippet(query, limit, MatchAll, filter, snippetTokens)
	if err != nil {
		return GroupedSearch{}, err
	}
	if len(matches) > 0 {
		return GroupedSearch{Matches: matches, Mode: MatchAll, SkippedSessions: skipped}, nil
	}

	loose, looseSkipped, err := searchGroupedSnippet(query, limit, MatchAny, filter, snippetTokens)
	if err != nil {
		return GroupedSearch{}, err
	}
	result := GroupedSearch{
		Matches:             loose,
		Mode:                MatchAny,
		SkippedSessions:     skipped + looseSkipped,
		TermsWithoutMatches: termsWithoutMatches(query),
	}
	if len(loose) == 0 {
		// Nothing either way. Reporting "any" would suggest the fallback
		// widened something; it did not.
		result.Mode = MatchAll
	}
	return result, nil
}

// SearchGrouped performs a session-level search with intelligent ranking.
// Fetches message-level FTS5 results, groups by session in Go, then
// enriches with session metadata. Ranked by BM25 * temporal_decay * density_bonus.
func SearchGrouped(query string, limit int) ([]SessionMatch, error) {
	return searchGrouped(query, limit, MatchAll, SearchFilter{})
}

func searchGrouped(query string, limit int, mode MatchMode, filter SearchFilter) ([]SessionMatch, error) {
	matches, _, err := searchGroupedSnippet(query, limit, mode, filter, 0)
	return matches, err
}

// DefaultSnippetTokens is how much of a matching message comes back when the
// caller does not say. Wider snippets cost tokens in whatever reads them;
// reading the whole passage is what mnemo_session is for.
const DefaultSnippetTokens = 64

// MaxSnippetTokens caps what a caller can ask for. Past this, fetching the
// session is both cheaper and complete.
const MaxSnippetTokens = 256

func searchGroupedSnippet(query string, limit int, mode MatchMode, filter SearchFilter, snippetTokens int) ([]SessionMatch, int, error) {
	if limit <= 0 {
		limit = 5
	}
	if snippetTokens <= 0 {
		snippetTokens = DefaultSnippetTokens
	}
	if snippetTokens > MaxSnippetTokens {
		snippetTokens = MaxSnippetTokens
	}

	safeQuery, err := fts5MatchExprMode(query, mode)
	if err != nil {
		return nil, 0, err
	}

	// Grouping happens in SQL, over every matching row.
	//
	// It used to happen in Go over the best `limit * 10` rows, which let one
	// session crowd out the rest: a term appearing three hundred times in
	// one transcript filled the fetch, and sessions that also held it were
	// never seen. The counts were wrong for the same reason, since they
	// counted the rows fetched rather than the rows that matched.
	//
	// The CTE is MATERIALIZED because FTS5's bm25() and snippet() only work
	// in the query that reads the table directly, and SQLite would otherwise
	// flatten the CTE into the aggregate and refuse. Measured on the 113 MB
	// archive: 0.04s for an ordinary word, 0.64s for "the".
	//
	// More sessions are taken than asked for, because the composite score
	// below can reorder them, and trimming to `limit` before scoring would
	// drop a session the caller should have seen.
	sessionLimit := limit * 3
	if sessionLimit < 15 {
		sessionLimit = 15
	}

	where, filterArgs := filter.sqlWhere()
	args := append([]any{snippetTokens, safeQuery}, filterArgs...)
	args = append(args, sessionLimit)

	rows, err := db.Query(fmt.Sprintf(`
		WITH hits AS MATERIALIZED (
			SELECT m.session_id AS sid, m.role AS role,
				   bm25(messages_fts) AS rank,
				   snippet(messages_fts, 0, '⟪', '⟫', '...', ?) AS snip
			FROM messages_fts
			JOIN messages m ON messages_fts.rowid = m.id
			JOIN sessions s ON s.id = m.session_id
			WHERE messages_fts MATCH ?%s
		),
		best AS (
			SELECT sid, role, snip, rank,
				   ROW_NUMBER() OVER (
					   PARTITION BY sid ORDER BY (role = 'user') DESC, rank
				   ) AS rn
			FROM hits
		)
		SELECT h.sid,
			   COUNT(*) AS match_count,
			   SUM(CASE WHEN h.role = 'user' THEN 1 ELSE 0 END) AS user_hits,
			   MIN(h.rank) AS best_rank,
			   b.role, b.snip
		FROM hits h
		JOIN best b ON b.sid = h.sid AND b.rn = 1
		GROUP BY h.sid
		ORDER BY best_rank
		LIMIT ?
	`, where), args...)
	if err != nil {
		return nil, 0, fmt.Errorf("searching for %q: %w", query, err)
	}
	defer func() { _ = rows.Close() }()

	type grouped struct {
		id          string
		matchCount  int
		userHits    int
		bestRank    float64
		snippet     string
		snippetRole string
	}

	var found []grouped
	for rows.Next() {
		var g grouped
		if err := rows.Scan(&g.id, &g.matchCount, &g.userHits, &g.bestRank, &g.snippetRole, &g.snippet); err != nil {
			return nil, 0, fmt.Errorf("reading a result of %q: %w", query, err)
		}
		found = append(found, g)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("reading the results of %q: %w", query, err)
	}

	// Session metadata, one row each. A session that matched but whose row
	// cannot be read is counted and reported: it used to be dropped in
	// silence, so a database fault read as "nothing matched".
	skipped := 0
	matches := make([]SessionMatch, 0, len(found))
	for _, g := range found {
		sm := SessionMatch{
			SessionID:   g.id,
			MatchCount:  g.matchCount,
			BestRank:    g.bestRank,
			Snippet:     g.snippet,
			SnippetRole: g.snippetRole,
		}

		if err := fillSessionMeta(&sm); err != nil {
			log.Printf("mnemo: skipping session %s in results for %q: %v", g.id, query, err)
			skipped++
			continue
		}

		sm.FinalScore = compositeScore(g.bestRank, g.matchCount, g.userHits, sm.StartTime)
		matches = append(matches, sm)
	}

	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].FinalScore < matches[j].FinalScore
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}

	return matches, skipped, nil
}

// fillSessionMeta reads one session's own columns into a match.
func fillSessionMeta(sm *SessionMatch) error {
	hostExpr := "''"
	if sessionsRecordHost() {
		hostExpr = "COALESCE(host, '')"
	}

	var timeStr string
	err := db.QueryRow(fmt.Sprintf(`
		SELECT project, COALESCE(first_query, ''), message_count, tool,
			   %s, COALESCE(working_directory, ''),
			   COALESCE(start_time, indexed_at, '')
		FROM sessions WHERE id = ?
	`, hostExpr), sm.SessionID).
		Scan(&sm.Project, &sm.FirstQuery, &sm.MessageCount, &sm.Tool, &sm.Host,
			&sm.WorkingDirectory, &timeStr)
	if err != nil {
		return err
	}

	sm.StartTime = parseFlexibleTime(timeStr)
	return nil
}

// compositeScore ranks a session: BM25, plus a bonus for matching often and
// for matching what the person said rather than what a tool printed, decayed
// by age. More negative is better, following BM25.
func compositeScore(bestRank float64, matchCount, userHits int, start time.Time) float64 {
	temporalDecay := 0.5 // a session with no start time is neither fresh nor stale
	if !start.IsZero() {
		daysOld := math.Max(0, time.Since(start).Hours()/24)
		temporalDecay = math.Exp(-0.03 * daysOld)
	}

	densityBonus := math.Min(float64(matchCount)*0.05, 1.0)
	userBonus := math.Min(float64(userHits)*0.1, 1.0)

	return (bestRank - densityBonus - userBonus) * temporalDecay
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
