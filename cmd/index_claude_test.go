package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFixture creates a Claude Code project tree under a temp dir:
//
//	<tmp>/-home-tmm-proj/s1.jsonl                          main transcript
//	<tmp>/-home-tmm-proj/s1/tool-results/abc123.txt        spilled tool output
//	<tmp>/-home-tmm-proj/s1/tool-results/def456.txt        spilled output from a subagent
//	<tmp>/-home-tmm-proj/s1/subagents/agent-a1.jsonl       subagent transcript
//
// It returns the paths of the two transcripts.
func writeFixture(t *testing.T) (string, string) {
	t.Helper()
	proj := filepath.Join(t.TempDir(), "-home-tmm-proj")
	results := filepath.Join(proj, "s1", "tool-results")
	subagents := filepath.Join(proj, "s1", "subagents")
	for _, d := range []string{results, subagents} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(filepath.Join(results, "abc123.txt"), "SPILLED CONTENT LINE\n")
	mustWrite(filepath.Join(results, "def456.txt"), "SUBAGENT SPILL\n")

	// The pointer path is deliberately one that does not exist here, as when
	// a Windows tree is indexed from WSL. The base name is what matters.
	mainLines := []string{
		`{"type":"summary","summary":"skip me"}`,
		`{"type":"user","uuid":"u1","parentUuid":null,"timestamp":"2026-09-02T20:54:00.000Z","cwd":"/home/tmm/proj","gitBranch":"main","version":"2.1.258","sessionId":"s1","message":{"role":"user","content":"hello world"}}`,
		`{"type":"attachment","uuid":"x1","timestamp":"2026-09-02T20:54:01.000Z"}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-09-02T20:54:02.000Z","sessionId":"s1","message":{"role":"assistant","model":"claude-fable-5-1","usage":{"input_tokens":100,"output_tokens":20},"content":[` +
			`{"type":"thinking","thinking":"pondering deeply","signature":"sig"},` +
			`{"type":"text","text":"I'll run ls"},` +
			`{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls -l","description":"List files"}}]}}`,
		`{"type":"user","uuid":"u2","parentUuid":"a1","timestamp":"2026-09-02T20:54:03.000Z","sessionId":"s1","toolUseResult":{"stdout":"ignored"},"message":{"role":"user","content":[{"tool_use_id":"t1","type":"tool_result","content":"AGENTS.md\nCHANGELOG.md"}]}}`,
		`{"type":"assistant","uuid":"a2","parentUuid":"u2","timestamp":"2026-09-02T20:54:04.000Z","sessionId":"s1","message":{"role":"assistant","model":"claude-fable-5-1","usage":{"input_tokens":50,"output_tokens":5},"content":[` +
			`{"type":"tool_use","id":"t2","name":"Read","input":{"file_path":"/home/tmm/proj/big.txt","limit":10}}]}}`,
		`{"type":"user","uuid":"u3","parentUuid":"a2","timestamp":"2026-09-02T20:54:05.000Z","sessionId":"s1","message":{"role":"user","content":[{"tool_use_id":"t2","type":"tool_result","content":[{"type":"text","text":"<persisted-output>\nOutput too large (45.8KB). Full output saved to: C:\\Users\\tmm\\.claude\\projects\\-home-tmm-proj\\s1\\tool-results\\abc123.txt"}]}]}}`,
		`{"type":"user","uuid":"u4","parentUuid":"u3","timestamp":"2026-09-02T20:54:06.000Z","sessionId":"s1","message":{"role":"user","content":[{"tool_use_id":"t3","type":"tool_result","content":[{"type":"image","source":{}}]}]}}`,
		`{"type":"assistant","uuid":"a3","parentUuid":"u4","timestamp":"2026-09-02T20:54:07.000Z","sessionId":"s1","message":{"role":"assistant","model":"claude-fable-5-1","usage":{"input_tokens":1,"output_tokens":1},"content":[{"type":"text","text":"Done."}]}}`,
	}
	mainPath := filepath.Join(proj, "s1.jsonl")
	mustWrite(mainPath, strings.Join(mainLines, "\n")+"\n")

	subLines := []string{
		`{"type":"user","uuid":"su1","parentUuid":null,"isSidechain":true,"agentId":"a1","sessionId":"s1","timestamp":"2026-09-02T20:55:00.000Z","cwd":"/home/tmm/proj","message":{"role":"user","content":"Trace what mnemo indexes"}}`,
		`{"type":"assistant","uuid":"sa1","parentUuid":"su1","isSidechain":true,"agentId":"a1","sessionId":"s1","timestamp":"2026-09-02T20:55:01.000Z","message":{"role":"assistant","model":"claude-fable-5-1","usage":{"input_tokens":10,"output_tokens":2},"content":[{"type":"tool_use","id":"st1","name":"Grep","input":{"pattern":"jsonl"}}]}}`,
		`{"type":"user","uuid":"su2","parentUuid":"sa1","isSidechain":true,"agentId":"a1","sessionId":"s1","timestamp":"2026-09-02T20:55:02.000Z","message":{"role":"user","content":[{"tool_use_id":"st1","type":"tool_result","content":"<persisted-output>\nOutput too large (50KB). Full output saved to: /home/tmm/.claude/projects/-home-tmm-proj/s1/tool-results/def456.txt"}]}}`,
		`{"type":"assistant","uuid":"sa2","parentUuid":"su2","isSidechain":true,"agentId":"a1","sessionId":"s1","timestamp":"2026-09-02T20:55:03.000Z","message":{"role":"assistant","model":"claude-fable-5-1","usage":{"input_tokens":10,"output_tokens":2},"content":[{"type":"text","text":"Findings below."}]}}`,
	}
	subPath := filepath.Join(subagents, "agent-a1.jsonl")
	mustWrite(subPath, strings.Join(subLines, "\n")+"\n")

	return mainPath, subPath
}

func roles(s *claudeSession) []string {
	out := make([]string, 0, len(s.Messages))
	for _, m := range s.Messages {
		out = append(out, m.Role)
	}
	return out
}

func TestParseClaudeSessionMain(t *testing.T) {
	mainPath, _ := writeFixture(t)

	s, err := parseClaudeSession(mainPath, "claude")
	if err != nil {
		t.Fatalf("parseClaudeSession: %v", err)
	}

	if s.ID != "s1" {
		t.Errorf("ID = %q, want s1", s.ID)
	}
	if s.ParentID != "" {
		t.Errorf("ParentID = %q, want empty for a main transcript", s.ParentID)
	}
	if s.FirstQuery != "hello world" {
		t.Errorf("FirstQuery = %q, want the first user text, not a tool result", s.FirstQuery)
	}

	want := []string{"user", "thinking", "assistant", "tool_use", "tool_result", "tool_use", "tool_result", "assistant"}
	got := roles(s)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("roles = %v, want %v", got, want)
	}

	m := s.Messages
	if m[1].Content != "pondering deeply" {
		t.Errorf("thinking content = %q", m[1].Content)
	}
	if !strings.Contains(m[3].Content, "Bash") || !strings.Contains(m[3].Content, "ls -l") {
		t.Errorf("tool_use content should carry the tool name and its input, got %q", m[3].Content)
	}
	if m[4].Content != "AGENTS.md\nCHANGELOG.md" {
		t.Errorf("tool_result string content = %q", m[4].Content)
	}
	if !strings.Contains(m[5].Content, "/home/tmm/proj/big.txt") || !strings.Contains(m[5].Content, "10") {
		t.Errorf("tool_use content should render non-string inputs too, got %q", m[5].Content)
	}
	if !strings.Contains(m[6].Content, "SPILLED CONTENT LINE") {
		t.Errorf("tool_result should include the spilled file found beside the session, got %q", m[6].Content)
	}
	if !strings.Contains(m[6].Content, "Full output saved to") {
		t.Errorf("tool_result should keep the pointer text, got %q", m[6].Content)
	}

	// All rows from one JSONL record share its identity fields.
	for _, i := range []int{1, 2, 3} {
		if m[i].MessageUUID != "a1" || m[i].ParentUUID != "u1" {
			t.Errorf("row %d: uuid/parent = %q/%q, want a1/u1", i, m[i].MessageUUID, m[i].ParentUUID)
		}
		if m[i].Model != "claude-fable-5-1" {
			t.Errorf("row %d: model = %q", i, m[i].Model)
		}
	}
	// Token usage is counted once per record, not once per row.
	if s.InputTokens != 151 || s.OutputTokens != 26 {
		t.Errorf("tokens = %d/%d, want 151/26", s.InputTokens, s.OutputTokens)
	}
	var rowIn, rowOut int
	for _, msg := range m {
		rowIn += msg.InputTokens
		rowOut += msg.OutputTokens
	}
	if rowIn != 151 || rowOut != 26 {
		t.Errorf("row token sum = %d/%d, want 151/26", rowIn, rowOut)
	}

	for i, msg := range m {
		if msg.Agent != "" {
			t.Errorf("row %d: Agent = %q, want empty for a main transcript", i, msg.Agent)
		}
		if msg.SessionID != "s1" || msg.Tool != "claude" {
			t.Errorf("row %d: session/tool = %q/%q", i, msg.SessionID, msg.Tool)
		}
	}
	if s.Cwd != "/home/tmm/proj" || s.GitBranch != "main" || s.Version != "2.1.258" {
		t.Errorf("session metadata = %q %q %q", s.Cwd, s.GitBranch, s.Version)
	}
	if s.Model != "claude-fable-5-1" {
		t.Errorf("Model = %q", s.Model)
	}
}

func TestParseClaudeSessionSubagent(t *testing.T) {
	mainPath, subPath := writeFixture(t)

	main, err := parseClaudeSession(mainPath, "claude")
	if err != nil {
		t.Fatal(err)
	}
	s, err := parseClaudeSession(subPath, "claude")
	if err != nil {
		t.Fatalf("parseClaudeSession: %v", err)
	}

	if s.ID != "agent-a1" {
		t.Errorf("ID = %q, want agent-a1", s.ID)
	}
	if s.ParentID != "s1" {
		t.Errorf("ParentID = %q, want s1", s.ParentID)
	}
	if s.Project != main.Project {
		t.Errorf("Project = %q, want %q (same as the parent)", s.Project, main.Project)
	}
	if s.FirstQuery != "Trace what mnemo indexes" {
		t.Errorf("FirstQuery = %q", s.FirstQuery)
	}

	want := []string{"user", "tool_use", "tool_result", "assistant"}
	if got := roles(s); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("roles = %v, want %v", got, want)
	}
	for i, msg := range s.Messages {
		if msg.Agent != "s1" {
			t.Errorf("row %d: Agent = %q, want the parent session id", i, msg.Agent)
		}
		if msg.SessionID != "agent-a1" {
			t.Errorf("row %d: SessionID = %q", i, msg.SessionID)
		}
	}
	// Spilled output for a subagent lives in the parent's tool-results dir.
	if !strings.Contains(s.Messages[2].Content, "SUBAGENT SPILL") {
		t.Errorf("subagent tool_result should resolve the spill via ../tool-results, got %q", s.Messages[2].Content)
	}
}

func TestParseClaudeSessionEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := parseClaudeSession(path, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 0 {
		t.Errorf("expected no messages from an empty file, got %d", len(s.Messages))
	}
}

func TestRenderToolInput(t *testing.T) {
	got := renderToolInput("Bash", map[string]interface{}{
		"description": "List files",
		"command":     "ls -l",
		"timeout":     float64(5000),
	})
	want := "Bash\ncommand: ls -l\ndescription: List files\ntimeout: 5000"
	if got != want {
		t.Errorf("renderToolInput =\n%q\nwant\n%q", got, want)
	}
}

func TestSpilledResultPath(t *testing.T) {
	proj := t.TempDir()
	results := filepath.Join(proj, "s1", "tool-results")
	if err := os.MkdirAll(results, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(results, "abc.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	mainFile := filepath.Join(proj, "s1.jsonl")
	subFile := filepath.Join(proj, "s1", "subagents", "agent-1.jsonl")

	cases := []struct {
		name, sessionFile, sessionID, text, want string
	}{
		{"beside main transcript", mainFile, "s1",
			"<persisted-output>\nOutput too large. Full output saved to: /elsewhere/s1/tool-results/abc.txt", filepath.Join(results, "abc.txt")},
		{"beside subagent transcript", subFile, "agent-1",
			"Full output saved to: C:\\x\\tool-results\\abc.txt", filepath.Join(results, "abc.txt")},
		{"no pointer", mainFile, "s1", "plain output", ""},
		{"pointer to missing file", mainFile, "s1", "Full output saved to: /x/tool-results/nope.txt", ""},
	}
	for _, c := range cases {
		if got := spilledResultPath(c.sessionFile, c.sessionID, c.text); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// A pasted PDF arrives as an attachment record of tens of megabytes on one
// line. Records after it must still be indexed.
func TestParseClaudeSessionLongLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "long.jsonl")
	big := strings.Repeat("A", 11*1024*1024)
	lines := []string{
		`{"type":"user","uuid":"u1","timestamp":"2026-09-02T20:54:00.000Z","message":{"role":"user","content":"before"}}`,
		`{"type":"attachment","uuid":"x1","attachment":{"type":"file","content":{"type":"pdf","base64":"` + big + `"}}}`,
		`{"type":"user","uuid":"u2","timestamp":"2026-09-02T20:54:01.000Z","message":{"role":"user","content":"after"}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := parseClaudeSession(path, "claude")
	if err != nil {
		t.Fatalf("parseClaudeSession: %v", err)
	}
	if got := roles(s); strings.Join(got, ",") != "user,user" {
		t.Fatalf("roles = %v, want both user records", got)
	}
	if s.Messages[1].Content != "after" {
		t.Errorf("second row = %q, want the record after the long line", s.Messages[1].Content)
	}
}

// A final line without a newline is still a record.
func TestParseClaudeSessionNoTrailingNewline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonl.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user","uuid":"u1","message":{"role":"user","content":"tail"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := parseClaudeSession(path, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 1 || s.Messages[0].Content != "tail" {
		t.Errorf("messages = %+v", s.Messages)
	}
}
