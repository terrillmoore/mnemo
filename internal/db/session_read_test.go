package db

import (
	"testing"
	"time"
)

// seedTranscript writes one session with a conversation and the tool rows
// around it, in the order an adapter would.
func seedTranscript(t *testing.T) {
	t.Helper()

	start := time.Date(2026, 9, 18, 14, 0, 0, 0, time.UTC)
	if err := InsertSession(Session{
		ID: "sess-1", Project: "proj", Tool: "claude", MessageCount: 5,
		FirstQuery: "why does the second stage hang", WorkingDirectory: "/home/tmm/proj",
		StartTime: start, EndTime: start.Add(time.Hour),
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	rows := []struct{ role, content string }{
		{"user", "why does the second stage hang"},
		{"assistant", "Looking at the watchdog."},
		{"tool_use", "Bash\ncommand: dmesg | tail"},
		{"tool_result", "watchdog: BUG: soft lockup"},
		{"assistant", "The watchdog fires before the second stage starts."},
	}
	for _, r := range rows {
		if err := InsertMessage(Message{
			SessionID: "sess-1", Project: "proj", Role: r.role, Content: r.content,
		}); err != nil {
			t.Fatalf("InsertMessage(%s): %v", r.role, err)
		}
	}
}

func TestGetSessionReturnsItsOwnFields(t *testing.T) {
	defer setupTestDB(t)()
	seedTranscript(t)

	saved := Host()
	defer SetHost(saved)

	got, found, err := GetSession("sess-1")
	if err != nil || !found {
		t.Fatalf("GetSession: %v, found %v", err, found)
	}
	if got.Project != "proj" || got.WorkingDirectory != "/home/tmm/proj" {
		t.Errorf("project %q, working directory %q", got.Project, got.WorkingDirectory)
	}
	if got.FirstQuery != "why does the second stage hang" {
		t.Errorf("first query %q", got.FirstQuery)
	}
	if got.StartTime.IsZero() {
		t.Error("start time is zero; the row records one")
	}
}

// A missing session is a miss, not an error: the id may belong to another
// machine's index, and the caller should be told which it is.
func TestGetSessionReportsAMissAsAMiss(t *testing.T) {
	defer setupTestDB(t)()

	got, found, err := GetSession("no-such-session")
	if err != nil {
		t.Fatalf("GetSession returned an error for an unknown id: %v", err)
	}
	if found {
		t.Errorf("found = true for an unknown id, got %+v", got)
	}
}

// The conversation is the default a caller wants; tool_result rows hold
// command output and whole files.
func TestGetSessionMessagesFiltersByRole(t *testing.T) {
	defer setupTestDB(t)()
	seedTranscript(t)

	msgs, total, err := GetSessionMessages("sess-1", []string{"user", "assistant"}, 50, 0)
	if err != nil {
		t.Fatalf("GetSessionMessages: %v", err)
	}
	if total != 3 || len(msgs) != 3 {
		t.Fatalf("total %d, returned %d, want 3 and 3", total, len(msgs))
	}
	for _, m := range msgs {
		if m.Role != "user" && m.Role != "assistant" {
			t.Errorf("role %q leaked through the filter", m.Role)
		}
	}

	all, allTotal, err := GetSessionMessages("sess-1", nil, 50, 0)
	if err != nil {
		t.Fatalf("GetSessionMessages(nil roles): %v", err)
	}
	if allTotal != 5 || len(all) != 5 {
		t.Errorf("no filter gave total %d, returned %d, want 5 and 5", allTotal, len(all))
	}
}

func TestGetSessionMessagesPagesInOrder(t *testing.T) {
	defer setupTestDB(t)()
	seedTranscript(t)

	first, total, err := GetSessionMessages("sess-1", nil, 2, 0)
	if err != nil {
		t.Fatalf("GetSessionMessages: %v", err)
	}
	if total != 5 {
		t.Errorf("total = %d, want 5 whatever the page size", total)
	}
	if len(first) != 2 {
		t.Fatalf("first page holds %d rows, want 2", len(first))
	}

	second, _, err := GetSessionMessages("sess-1", nil, 2, 2)
	if err != nil {
		t.Fatalf("GetSessionMessages(offset 2): %v", err)
	}
	if len(second) != 2 {
		t.Fatalf("second page holds %d rows, want 2", len(second))
	}
	if second[0].ID <= first[1].ID {
		t.Errorf("pages overlap or run backwards: first ends at %d, second starts at %d", first[1].ID, second[0].ID)
	}

	past, _, err := GetSessionMessages("sess-1", nil, 2, 99)
	if err != nil {
		t.Fatalf("GetSessionMessages(offset 99): %v", err)
	}
	if len(past) != 0 {
		t.Errorf("a page past the end holds %d rows, want none", len(past))
	}
}

func TestGetSessionMessagesIsEmptyForAnUnknownSession(t *testing.T) {
	defer setupTestDB(t)()
	seedTranscript(t)

	msgs, total, err := GetSessionMessages("no-such-session", nil, 50, 0)
	if err != nil {
		t.Fatalf("GetSessionMessages: %v", err)
	}
	if total != 0 || len(msgs) != 0 {
		t.Errorf("total %d, returned %d, want none of either", total, len(msgs))
	}
}
