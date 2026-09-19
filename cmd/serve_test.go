package cmd

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Pilan-AI/mnemo/internal/db"
)

// marshal renders a reply the way the MCP server sends it, so a test reads
// what a client receives rather than a Go value.
func marshal(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshalling the reply: %v", err)
	}
	if !utf8.Valid(b) {
		t.Errorf("reply is not valid UTF-8: %q", b)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("reading the reply back: %v", err)
	}
	return m
}

func TestSearchResponseCarriesEveryField(t *testing.T) {
	start := time.Date(2026, 9, 16, 19, 17, 4, 0, time.UTC)
	r := db.SessionMatch{
		SessionID:        "a0083f99",
		Project:          "tmm-mnemo",
		FirstQuery:       "why does the second stage hang",
		MessageCount:     395,
		Tool:             "claude",
		Host:             "tmmnote15",
		WorkingDirectory: "/home/tmm/mnemo",
		StartTime:        start,
		MatchCount:       7,
		FinalScore:       -19.357,
		Snippet:          "the ⟪second⟫ stage",
		SnippetRole:      "tool_result",
	}

	m := marshal(t, searchResponse{
		Query:   "second stage",
		Count:   1,
		Results: newSessionHits([]db.SessionMatch{r}),
	})

	if m["query"] != "second stage" {
		t.Errorf("query = %v", m["query"])
	}
	if m["project_filter"] != nil {
		t.Errorf("project_filter = %v, want null when no filter was given", m["project_filter"])
	}

	hits, ok := m["results"].([]any)
	if !ok || len(hits) != 1 {
		t.Fatalf("results = %v", m["results"])
	}
	hit := hits[0].(map[string]any)

	want := map[string]any{
		"session_id":        "a0083f99",
		"host":              "tmmnote15",
		"project":           "tmm-mnemo",
		"working_directory": "/home/tmm/mnemo",
		"tool":              "claude",
		"started_at":        "2026-09-16T19:17:04Z",
		"message_count":     float64(395),
		"match_count":       float64(7),
		"score":             -19.357,
		"first_query":       "why does the second stage hang",
		"snippet":           "the ⟪second⟫ stage",
		"snippet_role":      "tool_result",
	}
	for k, v := range want {
		if hit[k] != v {
			t.Errorf("%s = %#v, want %#v", k, hit[k], v)
		}
	}
	if _, present := hit["age_days"]; !present {
		t.Error("age_days is missing")
	}
}

// A machine the database does not record is null, not the machine that ran
// the query and not an empty string a caller might read as a name.
func TestSessionHitReportsUnknownFieldsAsNull(t *testing.T) {
	m := marshal(t, newSessionHit(db.SessionMatch{
		SessionID: "s1",
		Project:   "proj",
		Tool:      "claude",
	}))

	for _, field := range []string{"host", "working_directory", "started_at", "age_days", "first_query", "snippet", "snippet_role"} {
		v, present := m[field]
		if !present {
			t.Errorf("%s is missing; an unknown value is null, not absent", field)
			continue
		}
		if v != nil {
			t.Errorf("%s = %#v, want null", field, v)
		}
	}
}

// Nothing is cut to fit a width, so multi-byte text arrives whole. The old
// text format sliced the title by bytes and could split a rune in half.
func TestSessionHitKeepsMultiByteTextWhole(t *testing.T) {
	title := strings.Repeat("λ日本語→", 40) // 200 runes, 640 bytes
	m := marshal(t, newSessionHit(db.SessionMatch{
		SessionID:  "s1",
		Project:    "proj",
		Tool:       "claude",
		FirstQuery: title,
	}))

	got, ok := m["first_query"].(string)
	if !ok {
		t.Fatalf("first_query = %#v", m["first_query"])
	}
	if got != title {
		t.Errorf("first_query was altered: %d runes out, %d in", utf8.RuneCountInString(got), utf8.RuneCountInString(title))
	}
}

// An empty result is an empty list, so a caller needs no special case and no
// prose to interpret.
func TestEmptyRepliesCarryEmptyLists(t *testing.T) {
	m := marshal(t, searchResponse{Query: "nothing", Count: 0, Results: newSessionHits(nil)})
	if results, ok := m["results"].([]any); !ok || len(results) != 0 {
		t.Errorf("results = %#v, want []", m["results"])
	}

	m = marshal(t, recentResponse{Limit: 10, Count: 0, Sessions: newRecentEntries(nil)})
	if sessions, ok := m["sessions"].([]any); !ok || len(sessions) != 0 {
		t.Errorf("sessions = %#v, want []", m["sessions"])
	}

	m = marshal(t, newToolsResponse(nil))
	if tools, ok := m["tools"].([]any); !ok || len(tools) != 0 {
		t.Errorf("tools = %#v, want []", m["tools"])
	}
}

func TestRecentEntryCarriesTheMachineAndDirectory(t *testing.T) {
	indexed := time.Date(2026, 9, 19, 1, 34, 0, 0, time.UTC)
	start := time.Date(2026, 9, 18, 14, 0, 0, 0, time.UTC)

	entries := newRecentEntries([]db.RecentSession{{
		ID: "s1", Project: "standard-tools", Tool: "claude",
		MessageCount: 12, IndexedAt: indexed, StartTime: start,
		Host: "mercury2-emb-ah3", WorkingDirectory: "/home/tmm/ss",
	}})
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
	m := marshal(t, entries[0])

	want := map[string]any{
		"session_id":        "s1",
		"host":              "mercury2-emb-ah3",
		"working_directory": "/home/tmm/ss",
		"indexed_at":        "2026-09-19T01:34:00Z",
		"started_at":        "2026-09-18T14:00:00Z",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %#v, want %#v", k, m[k], v)
		}
	}
}

func TestToolsResponseCountsWhatWasFound(t *testing.T) {
	m := marshal(t, newToolsResponse([]Tool{
		{Name: "Claude Code", Path: "/home/tmm/.claude", Installed: true},
		{Name: "Cursor", Installed: false},
	}))

	if m["count"] != float64(2) || m["installed"] != float64(1) {
		t.Errorf("count = %v, installed = %v; want 2 and 1", m["count"], m["installed"])
	}
	tools := m["tools"].([]any)
	if path := tools[1].(map[string]any)["path"]; path != nil {
		t.Errorf("path = %#v, want null for a tool that was not found", path)
	}
}

func TestAgeDaysCountsWholeDays(t *testing.T) {
	got := ageDays(time.Now().Add(-73 * time.Hour))
	if got == nil {
		t.Fatal("age_days is null for a known start time")
	}
	if *got != 3 {
		t.Errorf("age_days = %d, want 3", *got)
	}
	if ageDays(time.Time{}) != nil {
		t.Error("age_days for the zero time should be null")
	}
}

func TestRFC3339UTCNormalizesTheZone(t *testing.T) {
	zone := time.FixedZone("EDT", -4*60*60)
	got := rfc3339UTC(time.Date(2026, 9, 16, 15, 17, 4, 0, zone))
	if got == nil {
		t.Fatal("started_at is null for a known time")
	}
	if *got != "2026-09-16T19:17:04Z" {
		t.Errorf("started_at = %q, want the same instant in UTC", *got)
	}
	if rfc3339UTC(time.Time{}) != nil {
		t.Error("started_at for the zero time should be null")
	}
}
