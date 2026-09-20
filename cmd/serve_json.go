package cmd

import (
	"time"

	"github.com/Pilan-AI/mnemo/internal/db"
)

// The MCP tools answer in JSON. A model reads these replies, and a model
// never reports a parse error: given a compact text line it resolves any
// ambiguity silently and carries on, so a project name holding a colon or a
// first query holding a newline turns into a confident wrong answer. Named
// fields cost tokens and remove that class of failure.
//
// Rules the types below keep:
//   - Every field a caller may need is present in every reply. A value that
//     the database does not record is null, never a substitute.
//   - Nothing is truncated for width. Both variable-length fields are already
//     bounded at the source: first_query by the indexer, at 200 runes, and
//     snippet by SQLite's snippet(), at 64 tokens.
//   - A list is always a list, empty when nothing matched, so a caller needs
//     no special case for "none".

// sessionHit is one search result. mnemo_search and mnemo_context both return
// these, so a caller learns one shape.
type sessionHit struct {
	SessionID string `json:"session_id"`
	// Host is the machine the session ran on, null when the database does
	// not record one. It decides where the transcript can be read, which
	// matters on an archive holding several machines' history.
	Host *string `json:"host"`
	// Project is derived by the indexer and is not reliable across tools;
	// WorkingDirectory is the dependable identifier.
	Project          string  `json:"project"`
	WorkingDirectory *string `json:"working_directory"`
	Tool             string  `json:"tool"`
	// StartedAt is RFC 3339 in UTC, null when the session records no start
	// time. AgeDays counts whole days from StartedAt to now, and is
	// negative if the recorded time is in the future.
	StartedAt    *string `json:"started_at"`
	AgeDays      *int    `json:"age_days"`
	MessageCount int     `json:"message_count"`
	// MatchCount is how many rows of this session matched. Score is the
	// composite rank, more negative being a better match.
	MatchCount int     `json:"match_count"`
	Score      float64 `json:"score"`
	// FirstQuery is the session's opening prompt as the indexer stored it,
	// null when it stored none.
	FirstQuery *string `json:"first_query"`
	// Snippet is the matching text with the hit wrapped in ⟪ ⟫.
	// SnippetRole names the block it came from: user, assistant, tool_use,
	// tool_result or thinking. A tool_result hit is command output, not
	// something anyone said.
	Snippet     *string `json:"snippet"`
	SnippetRole *string `json:"snippet_role"`
}

// searchResponse answers mnemo_search.
type searchResponse struct {
	Query   string  `json:"query"`
	Project *string `json:"project_filter"`
	// Mode says how the query was matched: "all" means every term had to
	// appear in one message, "any" that this found nothing and the search
	// widened to messages holding some of them. A sentence usually lands in
	// "any", and its results are looser than the caller may assume.
	Mode string `json:"mode"`
	// TermsWithoutMatches holds the words that appear nowhere in the index,
	// so a caller can drop them rather than guess which one spoiled the
	// query. Empty unless matching every term found nothing.
	TermsWithoutMatches []string     `json:"terms_without_matches"`
	Count               int          `json:"count"`
	Results             []sessionHit `json:"results"`
}

// contextResponse answers mnemo_context.
type contextResponse struct {
	Project  string       `json:"project"`
	Count    int          `json:"count"`
	Sessions []sessionHit `json:"sessions"`
}

// recentEntry is one session in mnemo_recent, ordered by when mnemo indexed
// it rather than by when it ran.
type recentEntry struct {
	SessionID        string  `json:"session_id"`
	Host             *string `json:"host"`
	Project          string  `json:"project"`
	WorkingDirectory *string `json:"working_directory"`
	Tool             string  `json:"tool"`
	FirstQuery       *string `json:"first_query"`
	MessageCount     int     `json:"message_count"`
	IndexedAt        *string `json:"indexed_at"`
	StartedAt        *string `json:"started_at"`
	AgeDays          *int    `json:"age_days"`
	Model            *string `json:"model"`
	Provider         *string `json:"provider"`
}

// recentResponse answers mnemo_recent.
type recentResponse struct {
	Limit    int           `json:"limit"`
	Count    int           `json:"count"`
	Sessions []recentEntry `json:"sessions"`
}

// toolEntry is one AI coding tool mnemo looks for on this machine.
type toolEntry struct {
	Name      string  `json:"name"`
	Path      *string `json:"path"`
	Installed bool    `json:"installed"`
}

// toolsResponse answers mnemo_tools. Installed counts the tools found, of
// Count looked for.
type toolsResponse struct {
	Count     int         `json:"count"`
	Installed int         `json:"installed"`
	Tools     []toolEntry `json:"tools"`
}

// sessionMessage is one row of a transcript on the wire.
type sessionMessage struct {
	ID int64 `json:"id"`
	// Role is the block type the adapter recorded: user, assistant,
	// tool_use, tool_result or thinking. A tool_result hit is command
	// output or a file, not something anyone said.
	Role      string  `json:"role"`
	Content   string  `json:"content"`
	Timestamp *string `json:"timestamp"`
	// Agent holds the parent session's id when this row belongs to a
	// subagent transcript, null otherwise.
	Agent *string `json:"agent"`
}

// sessionResponse answers mnemo_session: the session's own fields, so a
// caller arriving with nothing but an id learns where it ran, and one page
// of its rows.
type sessionResponse struct {
	SessionID        string  `json:"session_id"`
	Host             *string `json:"host"`
	Project          string  `json:"project"`
	WorkingDirectory *string `json:"working_directory"`
	Tool             string  `json:"tool"`
	FirstQuery       *string `json:"first_query"`
	StartedAt        *string `json:"started_at"`
	EndedAt          *string `json:"ended_at"`
	AgeDays          *int    `json:"age_days"`
	// MessageCount is every row the session holds, of any role. Total is
	// how many match the roles asked for, and the page is Offset to
	// Offset+Returned of those.
	MessageCount int              `json:"message_count"`
	Roles        []string         `json:"roles"`
	Total        int              `json:"total"`
	Offset       int              `json:"offset"`
	Limit        int              `json:"limit"`
	Returned     int              `json:"returned"`
	HasMore      bool             `json:"has_more"`
	Messages     []sessionMessage `json:"messages"`
}

// newSessionMessages converts a page of rows for the wire. The slice is
// never nil, so a page past the end marshals as [] rather than null.
func newSessionMessages(rows []db.SessionMessage) []sessionMessage {
	out := make([]sessionMessage, 0, len(rows))
	for _, r := range rows {
		out = append(out, sessionMessage{
			ID:        r.ID,
			Role:      r.Role,
			Content:   r.Content,
			Timestamp: rfc3339UTC(r.Timestamp),
			Agent:     optString(r.Agent),
		})
	}
	return out
}

// newSessionResponse assembles the reply from the session, the page, and
// what was asked for.
func newSessionResponse(s db.SessionDetail, roles []string, total, offset, limit int, rows []db.SessionMessage) sessionResponse {
	messages := newSessionMessages(rows)
	return sessionResponse{
		SessionID:        s.ID,
		Host:             optString(s.Host),
		Project:          s.Project,
		WorkingDirectory: optString(s.WorkingDirectory),
		Tool:             s.Tool,
		FirstQuery:       optString(s.FirstQuery),
		StartedAt:        rfc3339UTC(s.StartTime),
		EndedAt:          rfc3339UTC(s.EndTime),
		AgeDays:          ageDays(s.StartTime),
		MessageCount:     s.MessageCount,
		Roles:            roles,
		Total:            total,
		Offset:           offset,
		Limit:            limit,
		Returned:         len(messages),
		HasMore:          offset+len(messages) < total,
		Messages:         messages,
	}
}

// optString returns a pointer to s, or nil when s is empty, so a field the
// database does not record marshals as null rather than as an empty string
// a caller might read as a value.
func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// rfc3339UTC renders t for the wire, nil when t is the zero time. RFC 3339
// with an uppercase T and Z is also valid ISO 8601, which leaves one reading.
func rfc3339UTC(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

// ageDays counts whole days from t to now, nil when t is the zero time.
func ageDays(t time.Time) *int {
	if t.IsZero() {
		return nil
	}
	d := int(time.Since(t).Hours() / 24)
	return &d
}

// newSessionHit converts one search result for the wire.
func newSessionHit(r db.SessionMatch) sessionHit {
	return sessionHit{
		SessionID:        r.SessionID,
		Host:             optString(r.Host),
		Project:          r.Project,
		WorkingDirectory: optString(r.WorkingDirectory),
		Tool:             r.Tool,
		StartedAt:        rfc3339UTC(r.StartTime),
		AgeDays:          ageDays(r.StartTime),
		MessageCount:     r.MessageCount,
		MatchCount:       r.MatchCount,
		Score:            r.FinalScore,
		FirstQuery:       optString(r.FirstQuery),
		Snippet:          optString(r.Snippet),
		SnippetRole:      optString(r.SnippetRole),
	}
}

// newSessionHits converts a whole result set. The slice is never nil, so an
// empty result marshals as [] rather than null.
func newSessionHits(results []db.SessionMatch) []sessionHit {
	hits := make([]sessionHit, 0, len(results))
	for _, r := range results {
		hits = append(hits, newSessionHit(r))
	}
	return hits
}

// newRecentEntries converts recent sessions for the wire.
func newRecentEntries(sessions []db.RecentSession) []recentEntry {
	entries := make([]recentEntry, 0, len(sessions))
	for _, s := range sessions {
		entries = append(entries, recentEntry{
			SessionID:        s.ID,
			Host:             optString(s.Host),
			Project:          s.Project,
			WorkingDirectory: optString(s.WorkingDirectory),
			Tool:             s.Tool,
			FirstQuery:       optString(s.FirstQuery),
			MessageCount:     s.MessageCount,
			IndexedAt:        rfc3339UTC(s.IndexedAt),
			StartedAt:        rfc3339UTC(s.StartTime),
			AgeDays:          ageDays(s.StartTime),
			Model:            optString(s.Model),
			Provider:         optString(s.Provider),
		})
	}
	return entries
}

// newToolsResponse converts the tool scan for the wire.
func newToolsResponse(tools []Tool) toolsResponse {
	entries := make([]toolEntry, 0, len(tools))
	installed := 0
	for _, t := range tools {
		if t.Installed {
			installed++
		}
		entries = append(entries, toolEntry{
			Name:      t.Name,
			Path:      optString(t.Path),
			Installed: t.Installed,
		})
	}
	return toolsResponse{Count: len(tools), Installed: installed, Tools: entries}
}
