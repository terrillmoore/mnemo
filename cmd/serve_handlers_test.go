package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pilan-AI/mnemo/internal/db"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/mark3labs/mcp-go/server"
)

// These tests drive the registered handlers the way a client does, through
// MCPServer.HandleMessage, so they cover the wiring the converter tests in
// serve_test.go cannot: parameter parsing, defaults, filtering, the error
// paths, and the envelope each tool returns.

// seedServer points the db package at a temporary database, fills it with
// sessions from two machines, and returns the MCP server to drive.
func seedServer(t *testing.T) *server.MCPServer {
	t.Helper()

	dir := t.TempDir()
	t.Setenv("MNEMO_DB", filepath.Join(dir, "test.db"))
	t.Setenv("HOME", dir) // detectTools looks under the home directory

	if err := db.InitDB(); err != nil {
		t.Fatalf("opening the test database: %v", err)
	}
	t.Cleanup(db.CloseDB)

	saved := db.Host()
	db.SetHost("tmmnote15")
	t.Cleanup(func() { db.SetHost(saved) })

	now := time.Now()
	type row struct{ role, content string }
	sessions := []struct {
		id, project, firstQuery, workdir, host string
		age                                    time.Duration
		messages                               []row
	}{
		{
			id: "sess-1", project: "lora-bootloader", firstQuery: "why does the second stage hang",
			workdir: `C:\ss\lora-bootloader`, host: "tmmnote15", age: time.Hour,
			messages: []row{
				{"user", "The second stage hangs waiting for the watchdog."},
				{"assistant", "Disable the watchdog and the second stage boots."},
			},
		},
		{
			id: "sess-2", project: "standard-tools", firstQuery: "bsdmake parallel jobs",
			workdir: "/home/tmm/ss", host: "mercury2-emb-ah3", age: 48 * time.Hour,
			messages: []row{{"user", "The watchdog is unrelated to bsdmake."}},
		},
	}

	for _, s := range sessions {
		start := now.Add(-s.age)
		if err := db.InsertSession(db.Session{
			ID: s.id, Project: s.project, FirstQuery: s.firstQuery,
			MessageCount: len(s.messages), Tool: "claude", WorkingDirectory: s.workdir,
			Host: s.host, StartTime: start, IndexedAt: start,
		}); err != nil {
			t.Fatalf("seeding %s: %v", s.id, err)
		}
		for _, m := range s.messages {
			if err := db.InsertMessage(db.Message{
				SessionID: s.id, Project: s.project, Role: m.role, Content: m.content,
			}); err != nil {
				t.Fatalf("seeding a message of %s: %v", s.id, err)
			}
		}
	}

	return newMCPServer()
}

// call sends one tools/call and returns the reply's structuredContent, the
// text block, and whether the server marked the result an error.
func call(t *testing.T, s *server.MCPServer, tool string, args map[string]any) (map[string]any, string, bool) {
	t.Helper()

	request := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": tool, "arguments": args},
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}

	reply, err := json.Marshal(s.HandleMessage(context.Background(), raw))
	if err != nil {
		t.Fatalf("marshalling the reply: %v", err)
	}

	var envelope struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			StructuredContent map[string]any `json:"structuredContent"`
			IsError           bool           `json:"isError"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(reply, &envelope); err != nil {
		t.Fatalf("reading the reply: %v", err)
	}
	if envelope.Error != nil {
		t.Fatalf("%s returned a protocol error: %s", tool, envelope.Error.Message)
	}

	text := ""
	if len(envelope.Result.Content) > 0 {
		text = envelope.Result.Content[0].Text
	}
	return envelope.Result.StructuredContent, text, envelope.Result.IsError
}

func TestSearchHandlerReturnsBothMachines(t *testing.T) {
	s := seedServer(t)

	got, text, isErr := call(t, s, "mnemo_search", map[string]any{"query": "watchdog"})
	if isErr {
		t.Fatalf("search reported an error: %s", text)
	}

	if got["query"] != "watchdog" {
		t.Errorf("query = %v", got["query"])
	}
	if got["count"] != float64(2) {
		t.Errorf("count = %v, want 2", got["count"])
	}

	hosts := map[string]bool{}
	for _, hit := range got["results"].([]any) {
		h := hit.(map[string]any)
		hosts[fmt.Sprint(h["host"])] = true
		if h["session_id"] == "" || h["session_id"] == nil {
			t.Error("a hit carries no session_id")
		}
	}
	for _, want := range []string{"tmmnote15", "mercury2-emb-ah3"} {
		if !hosts[want] {
			t.Errorf("no hit from %s; got %v", want, hosts)
		}
	}
}

// The text block exists for a client that reads no structured content, so it
// must carry the same data rather than a summary of it.
func TestSearchHandlerTextBlockMatchesTheStructure(t *testing.T) {
	s := seedServer(t)

	got, text, _ := call(t, s, "mnemo_search", map[string]any{"query": "watchdog"})

	var fromText map[string]any
	if err := json.Unmarshal([]byte(text), &fromText); err != nil {
		t.Fatalf("the text block is not JSON: %v", err)
	}
	if fmt.Sprint(fromText) != fmt.Sprint(got) {
		t.Errorf("text block and structuredContent differ:\n text: %v\n data: %v", fromText, got)
	}
}

func TestSearchHandlerHonoursLimit(t *testing.T) {
	s := seedServer(t)

	got, _, _ := call(t, s, "mnemo_search", map[string]any{"query": "watchdog", "limit": 1})
	if got["count"] != float64(1) {
		t.Errorf("count = %v, want 1", got["count"])
	}
	if n := len(got["results"].([]any)); n != 1 {
		t.Errorf("returned %d results, want 1", n)
	}
}

func TestSearchHandlerFiltersByProject(t *testing.T) {
	s := seedServer(t)

	got, _, _ := call(t, s, "mnemo_search", map[string]any{"query": "watchdog", "project": "standard-tools"})

	filters := got["filters"].(map[string]any)
	if filters["project"] != "standard-tools" {
		t.Errorf("filters.project = %v", filters["project"])
	}
	results := got["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("returned %d results, want only the filtered one", len(results))
	}
	if p := results[0].(map[string]any)["project"]; p != "standard-tools" {
		t.Errorf("project = %v, want standard-tools", p)
	}
}

// Host and working directory are the dependable filters, and the reply
// echoes what was applied so an empty result can be read without guessing.
func TestSearchHandlerFiltersByMachineAndDirectory(t *testing.T) {
	s := seedServer(t)

	got, _, _ := call(t, s, "mnemo_search", map[string]any{
		"query": "watchdog", "host": "mercury2-emb-ah3",
	})
	results := got["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("host filter returned %d results, want 1", len(results))
	}
	if h := results[0].(map[string]any)["host"]; h != "mercury2-emb-ah3" {
		t.Errorf("host = %v", h)
	}

	// A fragment of the Windows path, which is what a caller has to hand.
	got, _, _ = call(t, s, "mnemo_search", map[string]any{
		"query": "watchdog", "working_directory": `ss\lora`,
	})
	results = got["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("working_directory filter returned %d results, want 1", len(results))
	}
	if h := results[0].(map[string]any)["host"]; h != "tmmnote15" {
		t.Errorf("host = %v, want the session in that directory", h)
	}

	filters := got["filters"].(map[string]any)
	if filters["working_directory"] == nil || filters["host"] != nil {
		t.Errorf("filters = %#v, want the directory set and the host null", filters)
	}
}

// A tool_use hit is a command someone ran, not a conclusion anyone reached,
// so a caller can ask for the conversation alone.
func TestSearchHandlerFiltersByRole(t *testing.T) {
	s := seedServer(t)

	got, _, _ := call(t, s, "mnemo_search", map[string]any{
		"query": "watchdog", "role": []any{"assistant"},
	})
	results := got["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("role filter returned %d results, want the 1 session with an assistant row", len(results))
	}
	if r := results[0].(map[string]any)["snippet_role"]; r != "assistant" {
		t.Errorf("snippet_role = %v, want assistant", r)
	}
}

// The date is the fixed-width part of a stored timestamp, so the comparison
// is safe even though the format varies after the seconds.
func TestSearchHandlerFiltersBySince(t *testing.T) {
	s := seedServer(t)

	recent, _, _ := call(t, s, "mnemo_search", map[string]any{
		"query": "watchdog", "since": time.Now().Add(-24 * time.Hour).Format("2006-01-02"),
	})
	if recent["count"] != float64(1) {
		t.Errorf("since yesterday found %v sessions, want the 1 from an hour ago", recent["count"])
	}

	all, _, _ := call(t, s, "mnemo_search", map[string]any{
		"query": "watchdog", "since": "2020-01-01",
	})
	if all["count"] != float64(2) {
		t.Errorf("since 2020 found %v sessions, want both", all["count"])
	}
}

func TestSearchHandlerAnswersEmptyWhenNothingMatches(t *testing.T) {
	s := seedServer(t)

	got, _, isErr := call(t, s, "mnemo_search", map[string]any{"query": "zzqqxx"})
	if isErr {
		t.Error("a query with no hits is not an error")
	}
	if got["count"] != float64(0) {
		t.Errorf("count = %v, want 0", got["count"])
	}
	if results, ok := got["results"].([]any); !ok || len(results) != 0 {
		t.Errorf("results = %#v, want []", got["results"])
	}
}

func TestSearchHandlerRejectsAMissingQuery(t *testing.T) {
	s := seedServer(t)

	_, text, isErr := call(t, s, "mnemo_search", map[string]any{})
	if !isErr {
		t.Fatalf("a missing query should be an error result; got %q", text)
	}
	if text == "" {
		t.Error("the error result says nothing")
	}
}

func TestContextHandlerReturnsTheProjectsSessions(t *testing.T) {
	s := seedServer(t)

	got, text, isErr := call(t, s, "mnemo_context", map[string]any{"project": "lora-bootloader"})
	if isErr {
		t.Fatalf("context reported an error: %s", text)
	}
	if got["project"] != "lora-bootloader" {
		t.Errorf("project = %v", got["project"])
	}
	sessions := got["sessions"].([]any)
	if len(sessions) == 0 {
		t.Fatal("no sessions returned")
	}
	for _, s := range sessions {
		if p := s.(map[string]any)["project"]; p != "lora-bootloader" {
			t.Errorf("project = %v, want only lora-bootloader", p)
		}
	}
}

func TestContextHandlerAnswersEmptyForAnUnknownProject(t *testing.T) {
	s := seedServer(t)

	got, _, isErr := call(t, s, "mnemo_context", map[string]any{"project": "no-such-project"})
	if isErr {
		t.Error("an unknown project is not an error")
	}
	if got["count"] != float64(0) {
		t.Errorf("count = %v, want 0", got["count"])
	}
	if sessions, ok := got["sessions"].([]any); !ok || len(sessions) != 0 {
		t.Errorf("sessions = %#v, want []", got["sessions"])
	}
}

func TestRecentHandlerCarriesMachineAndDirectory(t *testing.T) {
	s := seedServer(t)

	got, text, isErr := call(t, s, "mnemo_recent", map[string]any{"limit": 2})
	if isErr {
		t.Fatalf("recent reported an error: %s", text)
	}
	if got["limit"] != float64(2) {
		t.Errorf("limit = %v, want 2", got["limit"])
	}

	sessions := got["sessions"].([]any)
	if len(sessions) != 2 {
		t.Fatalf("returned %d sessions, want 2", len(sessions))
	}
	for _, entry := range sessions {
		e := entry.(map[string]any)
		if e["host"] == nil || e["working_directory"] == nil {
			t.Errorf("entry %v lost the machine or the directory", e["session_id"])
		}
		if e["indexed_at"] == nil {
			t.Errorf("entry %v has no indexed_at", e["session_id"])
		}
	}
}

func TestToolsHandlerReportsWhatItLookedFor(t *testing.T) {
	s := seedServer(t)

	got, text, isErr := call(t, s, "mnemo_tools", map[string]any{})
	if isErr {
		t.Fatalf("tools reported an error: %s", text)
	}
	count, ok := got["count"].(float64)
	if !ok || count == 0 {
		t.Fatalf("count = %v, want the number of tools looked for", got["count"])
	}
	if n := len(got["tools"].([]any)); float64(n) != count {
		t.Errorf("count says %v, list holds %d", count, n)
	}
}

// Every tool publishes an output schema at tools/list. A reply that does not
// satisfy it breaks the promise a client was given, so check each one against
// its own schema rather than trusting that the two were generated together.
func TestRepliesSatisfyTheirPublishedSchema(t *testing.T) {
	s := seedServer(t)

	raw, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	})
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	listed, err := json.Marshal(s.HandleMessage(context.Background(), raw))
	if err != nil {
		t.Fatalf("marshalling tools/list: %v", err)
	}

	var envelope struct {
		Result struct {
			Tools []struct {
				Name         string           `json:"name"`
				OutputSchema *json.RawMessage `json:"outputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(listed, &envelope); err != nil {
		t.Fatalf("reading tools/list: %v", err)
	}
	if len(envelope.Result.Tools) != 5 {
		t.Fatalf("tools/list returned %d tools, want 5", len(envelope.Result.Tools))
	}

	arguments := map[string]map[string]any{
		"mnemo_search":  {"query": "watchdog"},
		"mnemo_context": {"project": "lora-bootloader"},
		"mnemo_recent":  {"limit": 2},
		"mnemo_session": {"session_id": "sess-1"},
		"mnemo_tools":   {},
	}

	for _, tool := range envelope.Result.Tools {
		t.Run(tool.Name, func(t *testing.T) {
			if tool.OutputSchema == nil {
				t.Fatal("publishes no output schema")
			}

			var schema jsonschema.Schema
			if err := json.Unmarshal(*tool.OutputSchema, &schema); err != nil {
				t.Fatalf("reading the published schema: %v", err)
			}
			resolved, err := schema.Resolve(nil)
			if err != nil {
				t.Fatalf("resolving the published schema: %v", err)
			}

			args, ok := arguments[tool.Name]
			if !ok {
				t.Fatalf("no arguments in this test for %s", tool.Name)
			}
			got, _, isErr := call(t, s, tool.Name, args)
			if isErr {
				t.Fatalf("%s returned an error result", tool.Name)
			}
			if err := resolved.Validate(got); err != nil {
				t.Errorf("the reply does not satisfy the published schema: %v", err)
			}
		})
	}
}

// The point of the tool: a caller that holds no transcripts finds a session
// by searching and then reads it, without going to the machine it ran on.
func TestSessionHandlerReadsTheConversation(t *testing.T) {
	s := seedServer(t)

	got, text, isErr := call(t, s, "mnemo_session", map[string]any{"session_id": "sess-1"})
	if isErr {
		t.Fatalf("session reported an error: %s", text)
	}

	if got["session_id"] != "sess-1" || got["host"] != "tmmnote15" {
		t.Errorf("session_id %v, host %v", got["session_id"], got["host"])
	}
	if got["working_directory"] == nil {
		t.Error("working_directory is null; the row records one")
	}

	messages := got["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("returned %d messages, want the 2 seeded", len(messages))
	}
	first := messages[0].(map[string]any)
	if first["role"] != "user" {
		t.Errorf("first role = %v", first["role"])
	}
	if first["content"] == "" || first["content"] == nil {
		t.Error("a message came back with no content")
	}
	if got["has_more"] != false {
		t.Errorf("has_more = %v with the whole session returned", got["has_more"])
	}
}

// Asking for tool rows is how a caller reads what a command printed, and it
// is not the default because those rows are far larger than the talk.
func TestSessionHandlerHonoursRolesAndPaging(t *testing.T) {
	s := seedServer(t)

	got, _, _ := call(t, s, "mnemo_session", map[string]any{
		"session_id": "sess-1",
		"roles":      []any{"user"},
		"limit":      1,
	})

	if got["total"] != float64(1) {
		t.Errorf("total = %v, want the 1 user row", got["total"])
	}
	messages := got["messages"].([]any)
	if len(messages) != 1 || messages[0].(map[string]any)["role"] != "user" {
		t.Fatalf("messages = %#v", messages)
	}

	// Two rows in the conversation, one at a time.
	page, _, _ := call(t, s, "mnemo_session", map[string]any{
		"session_id": "sess-1", "limit": 1, "offset": 0,
	})
	if page["has_more"] != true {
		t.Errorf("has_more = %v on the first of two rows", page["has_more"])
	}
	if page["returned"] != float64(1) || page["total"] != float64(2) {
		t.Errorf("returned %v of total %v", page["returned"], page["total"])
	}
}

// An id from another machine's index, or a typo, is worth saying plainly
// rather than answering with an empty session.
func TestSessionHandlerRejectsAnUnknownSession(t *testing.T) {
	s := seedServer(t)

	_, text, isErr := call(t, s, "mnemo_session", map[string]any{"session_id": "no-such-session"})
	if !isErr {
		t.Fatalf("an unknown session should be an error result; got %q", text)
	}
	if !strings.Contains(text, "no-such-session") {
		t.Errorf("the error does not name the session: %q", text)
	}
}

// A sentence asks for every word in one message. The reply says which match
// produced the results and which words are not in the index at all, so the
// caller can narrow instead of concluding the corpus is empty.
func TestSearchHandlerSaysHowItMatched(t *testing.T) {
	s := seedServer(t)

	strict, _, _ := call(t, s, "mnemo_search", map[string]any{"query": "watchdog"})
	if strict["mode"] != "all" {
		t.Errorf("mode = %v, want all", strict["mode"])
	}
	if terms, ok := strict["terms_without_matches"].([]any); !ok || len(terms) != 0 {
		t.Errorf("terms_without_matches = %#v, want [] when the strict pass answered", strict["terms_without_matches"])
	}

	loose, _, _ := call(t, s, "mnemo_search", map[string]any{
		"query": "watchdog zzqqxx",
	})
	if loose["mode"] != "any" {
		t.Errorf("mode = %v, want any once the strict pass found nothing", loose["mode"])
	}
	if loose["count"] == float64(0) {
		t.Error("the fallback returned nothing; the corpus holds watchdog")
	}
	terms := loose["terms_without_matches"].([]any)
	if len(terms) != 1 || terms[0] != "zzqqxx" {
		t.Errorf("terms_without_matches = %#v, want the one word that is not in the index", terms)
	}
}

// The snippet width is the caller's to choose, within reason: past the cap,
// fetching the session with mnemo_session is cheaper and complete.
func TestSearchHandlerWidensTheSnippet(t *testing.T) {
	s := seedServer(t)

	narrow, _, _ := call(t, s, "mnemo_search", map[string]any{
		"query": "watchdog", "snippet_tokens": 4,
	})
	wide, _, _ := call(t, s, "mnemo_search", map[string]any{
		"query": "watchdog", "snippet_tokens": 64,
	})

	short := narrow["results"].([]any)[0].(map[string]any)["snippet"].(string)
	long := wide["results"].([]any)[0].(map[string]any)["snippet"].(string)
	if len(short) >= len(long) {
		t.Errorf("4 tokens gave %d bytes, 64 gave %d; the parameter did nothing", len(short), len(long))
	}

	// Above the cap the search still answers, with the cap applied.
	huge, _, isErr := call(t, s, "mnemo_search", map[string]any{
		"query": "watchdog", "snippet_tokens": 100000,
	})
	if isErr {
		t.Error("an oversized snippet_tokens should be capped, not refused")
	}
	if huge["count"] == float64(0) {
		t.Error("the capped search returned nothing")
	}
}

// A raw BM25 score means nothing to a reader; its position in the ordering
// does.
func TestSearchHandlerRanksHitsFromOne(t *testing.T) {
	s := seedServer(t)

	got, _, _ := call(t, s, "mnemo_search", map[string]any{"query": "watchdog"})

	results := got["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("returned %d results, want 2", len(results))
	}
	for i, hit := range results {
		h := hit.(map[string]any)
		if h["rank"] != float64(i+1) {
			t.Errorf("result %d has rank %v", i, h["rank"])
		}
		if _, present := h["score"]; present {
			t.Error("score is still in the reply")
		}
	}
}
