package db

import (
	"fmt"
	"testing"
	"time"
)

func TestFTS5MatchExpr(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "one word", input: "authentication", want: `"authentication"`},
		{name: "two words are ANDed", input: "auth flow", want: `"auth" AND "flow"`},
		// The characters that used to be stripped, or to kill the query.
		{name: "full stop", input: "V1.15", want: `"V1.15"`},
		{name: "leading dash", input: "-j all", want: `"-j" AND "all"`},
		{name: "plus signs", input: "C++ code", want: `"C++" AND "code"`},
		{name: "path with a colon", input: "mcci/tools:bin", want: `"mcci/tools:bin"`},
		{name: "parentheses", input: "func(x)", want: `"func(x)"`},
		{name: "apostrophe", input: "it's working", want: `"it's" AND "working"`},
		// A quote in the input is doubled, so it cannot end the string early.
		{name: "embedded quote", input: `say "hi"`, want: `"say" AND """hi"""`},
		{name: "collapses whitespace", input: "  a\t b  ", want: `"a" AND "b"`},
		// Nothing to tokenize: dropped rather than quoted into an empty phrase.
		{name: "punctuation only term", input: "auth -- flow", want: `"auth" AND "flow"`},
		{name: "punctuation only query", input: "???***", wantErr: true},
		{name: "empty", input: "   ", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := fts5MatchExpr(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("fts5MatchExpr(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("fts5MatchExpr(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// The failure this replaces: a full stop reached FTS5 and the search died
// with a SQLite syntax error rather than returning anything.
func TestSearchAcceptsPunctuation(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	_ = InsertSession(Session{
		ID: "sess-1", Project: "proj", Tool: "claude", MessageCount: 1,
		StartTime: time.Now().Add(-time.Hour),
	})
	_ = InsertMessage(Message{
		SessionID: "sess-1", Project: "proj", Role: "user",
		Content: "bsdmake V1.15 built the parallel tree with -j 8",
	})

	for _, query := range []string{"V1.15", "bsdmake V1.15", "-j 8", "C++", "mcci/tools:bin"} {
		if _, err := SearchGrouped(query, 5); err != nil {
			t.Errorf("SearchGrouped(%q) = %v, want no error", query, err)
		}
		if _, err := Search(query, 5); err != nil {
			t.Errorf("Search(%q) = %v, want no error", query, err)
		}
	}

	results, err := SearchGrouped("V1.15", 5)
	if err != nil {
		t.Fatalf("SearchGrouped: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("V1.15 found %d sessions, want the 1 holding it", len(results))
	}
}

func TestParseFlexibleTime(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantUTC  bool
		wantZero bool
	}{
		{name: "empty string", input: "", wantZero: true},
		{name: "sqlite format", input: "2026-02-08 14:30:00", wantUTC: true},
		{name: "RFC3339", input: "2026-02-08T14:30:00Z", wantUTC: true},
		{name: "RFC3339 with offset", input: "2026-02-08T14:30:00+05:30", wantUTC: true},
		{name: "Go timestamp with tz", input: "2026-02-08 14:30:00.000 +0000 UTC", wantUTC: true},
		{name: "ISO without T", input: "2026-02-08T14:30:00Z", wantUTC: true},
		{name: "invalid format", input: "not-a-date", wantZero: true},
		{name: "partial date", input: "2026-02", wantZero: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseFlexibleTime(tt.input)
			if tt.wantZero {
				if !got.IsZero() {
					t.Errorf("parseFlexibleTime(%q) = %v, want zero", tt.input, got)
				}
				return
			}
			if got.IsZero() {
				t.Errorf("parseFlexibleTime(%q) returned zero, want non-zero", tt.input)
				return
			}
			if tt.wantUTC && got.Location() != time.UTC {
				t.Errorf("parseFlexibleTime(%q) location = %v, want UTC", tt.input, got.Location())
			}
		})
	}
}

func TestParseFlexibleTimeUTCNormalization(t *testing.T) {
	// A timestamp with +05:30 offset should be normalized to UTC
	result := parseFlexibleTime("2026-02-08T14:30:00+05:30")
	if result.IsZero() {
		t.Fatal("expected non-zero result")
	}
	if result.Hour() != 9 {
		t.Errorf("expected UTC hour 9 (14:30 IST = 09:00 UTC), got %d", result.Hour())
	}
}

func TestSearch(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	_ = InsertSessionSimple("sess-1", "proj", "auth question", "/p", "claude", 2)
	_ = InsertMessage(Message{SessionID: "sess-1", Project: "proj", Role: "user", Content: "How to implement JWT authentication?"})
	_ = InsertMessage(Message{SessionID: "sess-1", Project: "proj", Role: "assistant", Content: "Here is how to implement JWT authentication in Go."})

	_ = InsertSessionSimple("sess-2", "proj", "database design", "/p", "claude", 1)
	_ = InsertMessage(Message{SessionID: "sess-2", Project: "proj", Role: "user", Content: "Help me design the database schema."})

	results, err := Search("authentication", 10)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}

	if len(results) != 2 {
		t.Errorf("expected 2 results for 'authentication', got %d", len(results))
	}
}

func TestSearchNoResults(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	_ = InsertSessionSimple("sess-1", "proj", "q", "/p", "claude", 1)
	_ = InsertMessage(Message{SessionID: "sess-1", Project: "proj", Role: "user", Content: "hello world"})

	results, err := Search("nonexistent_term_xyz", 10)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}

	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}

func TestSearchLimit(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	_ = InsertSessionSimple("sess-1", "proj", "q", "/p", "claude", 5)
	for i := 0; i < 5; i++ {
		_ = InsertMessage(Message{
			SessionID: "sess-1", Project: "proj", Role: "user",
			Content: "authentication implementation discussion",
		})
	}

	results, err := Search("authentication", 2)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}

	if len(results) != 2 {
		t.Errorf("expected 2 results (limited), got %d", len(results))
	}
}

func TestSearchEmptyQuery(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	_, err := Search("", 10)
	if err == nil {
		t.Error("Search with empty query should return error")
	}
}

func TestSearchSpecialChars(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	_ = InsertSessionSimple("sess-1", "proj", "q", "/p", "claude", 1)
	_ = InsertMessage(Message{SessionID: "sess-1", Project: "proj", Role: "user", Content: "function call test"})

	// Should not error even with special chars
	results, err := Search("function()", 10)
	if err != nil {
		t.Fatalf("Search with special chars error = %v", err)
	}

	if len(results) != 1 {
		t.Errorf("expected 1 result, got %d", len(results))
	}
}

func TestSearchGrouped(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	now := time.Now()

	// Session 1: 3 messages about auth
	_ = InsertSession(Session{
		ID: "sess-1", Project: "proj-a", FirstQuery: "auth question",
		MessageCount: 3, Tool: "claude", StartTime: now.Add(-1 * time.Hour),
	})
	_ = InsertMessage(Message{SessionID: "sess-1", Project: "proj-a", Role: "user", Content: "How to implement authentication?"})
	_ = InsertMessage(Message{SessionID: "sess-1", Project: "proj-a", Role: "assistant", Content: "Use JWT for authentication."})
	_ = InsertMessage(Message{SessionID: "sess-1", Project: "proj-a", Role: "user", Content: "Show authentication middleware."})

	// Session 2: 1 message about auth
	_ = InsertSession(Session{
		ID: "sess-2", Project: "proj-b", FirstQuery: "login",
		MessageCount: 1, Tool: "opencode", StartTime: now.Add(-24 * time.Hour),
	})
	_ = InsertMessage(Message{SessionID: "sess-2", Project: "proj-b", Role: "user", Content: "Authentication flow for login page."})

	results, err := SearchGrouped("authentication", 5)
	if err != nil {
		t.Fatalf("SearchGrouped() error = %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 session results, got %d", len(results))
	}

	// Session with more matches and more recent should rank higher
	if results[0].SessionID != "sess-1" {
		t.Errorf("expected sess-1 to rank first (more matches, more recent), got %s", results[0].SessionID)
	}

	if results[0].MatchCount < results[1].MatchCount {
		t.Errorf("first result should have more matches: %d vs %d", results[0].MatchCount, results[1].MatchCount)
	}
}

func TestSearchGroupedLimit(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	now := time.Now()
	for i := 0; i < 10; i++ {
		sid := fmt.Sprintf("sess-%d", i)
		_ = InsertSession(Session{
			ID: sid, Project: "proj", FirstQuery: "test query",
			MessageCount: 1, Tool: "claude", StartTime: now.Add(-time.Duration(i) * time.Hour),
		})
		_ = InsertMessage(Message{SessionID: sid, Project: "proj", Role: "user", Content: "common keyword search term"})
	}

	results, err := SearchGrouped("keyword", 3)
	if err != nil {
		t.Fatalf("SearchGrouped() error = %v", err)
	}

	if len(results) != 3 {
		t.Errorf("expected 3 results (limited), got %d", len(results))
	}
}

func TestSearchGroupedZeroLimit(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	_ = InsertSession(Session{
		ID: "sess-1", Project: "proj", FirstQuery: "q",
		MessageCount: 1, Tool: "claude", StartTime: time.Now(),
	})
	_ = InsertMessage(Message{SessionID: "sess-1", Project: "proj", Role: "user", Content: "hello world"})

	// 0 defaults to 5
	results, err := SearchGrouped("hello", 0)
	if err != nil {
		t.Fatalf("SearchGrouped(0) error = %v", err)
	}

	if len(results) != 1 {
		t.Errorf("expected 1 result, got %d", len(results))
	}
}

func TestSearchGroupedUserSnippetPreference(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	_ = InsertSession(Session{
		ID: "sess-1", Project: "proj", FirstQuery: "q",
		MessageCount: 2, Tool: "claude", StartTime: time.Now(),
	})
	_ = InsertMessage(Message{SessionID: "sess-1", Project: "proj", Role: "assistant", Content: "database migration pattern"})
	_ = InsertMessage(Message{SessionID: "sess-1", Project: "proj", Role: "user", Content: "How to do database migration?"})

	results, err := SearchGrouped("database migration", 5)
	if err != nil {
		t.Fatalf("SearchGrouped() error = %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 session result, got %d", len(results))
	}

	// Should prefer user snippet
	if results[0].SnippetRole != "user" {
		t.Errorf("expected snippet from user role, got %s", results[0].SnippetRole)
	}
}

func TestSearchGroupedEmptyQuery(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	_, err := SearchGrouped("***", 5)
	if err == nil {
		t.Error("SearchGrouped with empty-after-sanitize query should error")
	}
}

// A database written only by upstream mnemo has no host column, and a
// read-only caller such as the MCP server cannot add one. Search must still
// answer, leaving the machine blank, rather than reporting that nothing
// matched.
func TestSearchGroupedWithoutHostColumn(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	_ = InsertSession(Session{
		ID: "sess-1", Project: "proj-a", FirstQuery: "auth question",
		MessageCount: 2, Tool: "claude", StartTime: time.Now().Add(-1 * time.Hour),
	})
	_ = InsertMessage(Message{SessionID: "sess-1", Project: "proj-a", Role: "user", Content: "How to implement authentication?"})
	_ = InsertMessage(Message{SessionID: "sess-1", Project: "proj-a", Role: "assistant", Content: "Use JWT for authentication."})

	if _, err := db.Exec("DROP INDEX IF EXISTS idx_sessions_host"); err != nil {
		t.Fatalf("dropping the host index: %v", err)
	}
	if _, err := db.Exec("ALTER TABLE sessions DROP COLUMN host"); err != nil {
		t.Fatalf("dropping the host column: %v", err)
	}

	results, err := SearchGrouped("authentication", 5)
	if err != nil {
		t.Fatalf("SearchGrouped() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 session result, got %d", len(results))
	}
	if results[0].Host != "" {
		t.Errorf("Host = %q, want empty for a database that records none", results[0].Host)
	}
}

// The same query against a database that does record the host names it.
func TestSearchGroupedReportsTheHost(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	saved := Host()
	defer SetHost(saved)
	SetHost("apollo")

	_ = InsertSession(Session{
		ID: "sess-1", Project: "proj-a", FirstQuery: "auth question",
		MessageCount: 2, Tool: "claude", StartTime: time.Now().Add(-1 * time.Hour),
	})
	_ = InsertMessage(Message{SessionID: "sess-1", Project: "proj-a", Role: "user", Content: "How to implement authentication?"})

	results, err := SearchGrouped("authentication", 5)
	if err != nil {
		t.Fatalf("SearchGrouped() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 session result, got %d", len(results))
	}
	if results[0].Host != "apollo" {
		t.Errorf("Host = %q, want %q", results[0].Host, "apollo")
	}
}

// The working directory identifies the work when the derived project name
// does not: two machines both hold /home/tmm/sales-pipeline, and the Windows
// tree mangles the path into the project name.
func TestSearchGroupedReportsTheWorkingDirectory(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	_ = InsertSession(Session{
		ID: "sess-1", Project: "proj-a", FirstQuery: "auth question",
		MessageCount: 1, Tool: "claude", WorkingDirectory: `C:\ss\proj-a`,
		StartTime: time.Now().Add(-1 * time.Hour),
	})
	_ = InsertMessage(Message{SessionID: "sess-1", Project: "proj-a", Role: "user", Content: "How to implement authentication?"})

	results, err := SearchGrouped("authentication", 5)
	if err != nil {
		t.Fatalf("SearchGrouped() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 session result, got %d", len(results))
	}
	if got, want := results[0].WorkingDirectory, `C:\ss\proj-a`; got != want {
		t.Errorf("WorkingDirectory = %q, want %q", got, want)
	}
}

// A question written as a sentence asks for every word in one message, which
// is the one thing that reliably finds nothing. The search says so rather
// than answering "no results" to a corpus that holds the answer.
func TestSearchGroupedExplainedFallsBackToAnyTerm(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	_ = InsertSession(Session{
		ID: "sess-1", Project: "proj", Tool: "claude", MessageCount: 1,
		StartTime: time.Now().Add(-time.Hour),
	})
	_ = InsertMessage(Message{
		SessionID: "sess-1", Project: "proj", Role: "user",
		Content: "the oak-libs build wrote its objects under the parallel tree",
	})

	strict, err := SearchGroupedExplained("oak-libs objects", 5, SearchFilter{})
	if err != nil {
		t.Fatalf("SearchGroupedExplained: %v", err)
	}
	if strict.Mode != MatchAll || len(strict.Matches) != 1 {
		t.Errorf("mode %q with %d matches, want all with 1", strict.Mode, len(strict.Matches))
	}
	if len(strict.TermsWithoutMatches) != 0 {
		t.Errorf("terms without matches = %v, want none when the strict pass answered", strict.TermsWithoutMatches)
	}

	loose, err := SearchGroupedExplained("oak-libs objects elapsed compile errors", 5, SearchFilter{})
	if err != nil {
		t.Fatalf("SearchGroupedExplained: %v", err)
	}
	if loose.Mode != MatchAny {
		t.Errorf("mode = %q, want any once the strict pass found nothing", loose.Mode)
	}
	if len(loose.Matches) != 1 {
		t.Errorf("found %d matches, want the 1 session holding some of the words", len(loose.Matches))
	}
	want := map[string]bool{"elapsed": true, "compile": true, "errors": true}
	for _, term := range loose.TermsWithoutMatches {
		if !want[term] {
			t.Errorf("reported %q as unmatched; it is in the corpus", term)
		}
		delete(want, term)
	}
	if len(want) != 0 {
		t.Errorf("did not report these as unmatched: %v", want)
	}
}

// Nothing either way: reporting "any" would suggest the fallback widened
// something, and it did not.
func TestSearchGroupedExplainedReportsStrictWhenNothingMatches(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	_ = InsertSession(Session{ID: "sess-1", Project: "proj", Tool: "claude", MessageCount: 1})
	_ = InsertMessage(Message{SessionID: "sess-1", Project: "proj", Role: "user", Content: "nothing to do with it"})

	got, err := SearchGroupedExplained("zzqqxx yyzzww", 5, SearchFilter{})
	if err != nil {
		t.Fatalf("SearchGroupedExplained: %v", err)
	}
	if len(got.Matches) != 0 {
		t.Fatalf("found %d matches, want none", len(got.Matches))
	}
	if got.Mode != MatchAll {
		t.Errorf("mode = %q, want all when neither pass found anything", got.Mode)
	}
	if len(got.TermsWithoutMatches) != 2 {
		t.Errorf("terms without matches = %v, want both", got.TermsWithoutMatches)
	}
}

// A term repeated hundreds of times in one transcript used to fill the fetch
// of best rows, and every other session holding it was never seen. Grouping
// in SQL means the loud session takes one place in the results, not all of
// them.
func TestSearchGroupedDoesNotLetOneSessionCrowdOthersOut(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	// One session that says "watchdog" 200 times.
	_ = InsertSession(Session{
		ID: "loud", Project: "proj-loud", Tool: "claude", MessageCount: 200,
		StartTime: time.Now().Add(-time.Hour),
	})
	for i := 0; i < 200; i++ {
		_ = InsertMessage(Message{
			SessionID: "loud", Project: "proj-loud", Role: "user",
			Content: "the watchdog fired again",
		})
	}

	// Three that mention it once.
	for _, id := range []string{"quiet-1", "quiet-2", "quiet-3"} {
		_ = InsertSession(Session{
			ID: id, Project: "proj-" + id, Tool: "claude", MessageCount: 1,
			StartTime: time.Now().Add(-2 * time.Hour),
		})
		_ = InsertMessage(Message{
			SessionID: id, Project: "proj-" + id, Role: "user",
			Content: "the watchdog is mentioned here once",
		})
	}

	results, err := SearchGrouped("watchdog", 10)
	if err != nil {
		t.Fatalf("SearchGrouped: %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("found %d sessions, want all 4", len(results))
	}

	for _, r := range results {
		if r.SessionID == "loud" && r.MatchCount != 200 {
			t.Errorf("the loud session reports %d hits, want the real 200", r.MatchCount)
		}
	}
}
