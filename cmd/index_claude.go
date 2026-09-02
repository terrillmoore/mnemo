// index_claude.go indexes Claude Code sessions stored as JSONL files.
// Session data lives under ~/.claude/projects/<project-hash>/<session>.jsonl.
// Subagent transcripts live beside the session in
// <session>/subagents/agent-<id>.jsonl, and tool output too large for the
// transcript is spilled to <session>/tool-results/<name>.txt.
package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Pilan-AI/mnemo/internal/db"
)

// maxSpilledResultBytes caps how much of a spilled tool-results file is
// indexed. Claude Code spills at about 45 KB, so this is only a guard.
const maxSpilledResultBytes = 16 * 1024 * 1024

// spilledResultRe matches the pointer Claude Code leaves in a tool_result
// when it moves the output to a file. The path may come from another
// machine, so only its base name is trusted.
var spilledResultRe = regexp.MustCompile(`Full output saved to: (\S+)`)

// indexClaudeCode walks the Claude Code projects directory and indexes each
// JSONL session file. Returns total (sessions, messages) indexed.
func indexClaudeCode(basePath string) (int, int) {
	sessions := 0
	messages := 0

	if err := filepath.Walk(basePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			indexErrors++
			return nil
		}

		if info.IsDir() {
			return nil
		}

		if strings.HasSuffix(info.Name(), ".jsonl") {
			if skipOldFile(info) {
				return nil
			}
			sessionID := strings.TrimSuffix(info.Name(), ".jsonl")
			if isSessionUnchanged(sessionID, info.ModTime()) {
				return nil
			}
			s, m := indexJSONLSession(path, "claude")
			sessions += s
			messages += m
		}

		return nil
	}); err != nil {
		indexErrors++
	}

	return sessions, messages
}

// claudeSession is one parsed Claude Code transcript: the session record's
// fields plus one db.Message per indexed content block.
type claudeSession struct {
	ID         string
	ParentID   string // parent session of a subagent transcript, else ""
	Project    string
	FirstQuery string
	Cwd        string
	GitBranch  string
	Version    string
	Model      string
	Provider   string

	InputTokens  int
	OutputTokens int
	StartTime    time.Time
	EndTime      time.Time

	Messages []db.Message
}

// indexJSONLSession parses a single JSONL session file and inserts all its
// messages atomically within a transaction.
func indexJSONLSession(path, tool string) (int, int) {
	s, err := parseClaudeSession(path, tool)
	if err != nil {
		// A read error part way through still leaves the records before it.
		indexErrors++
	}
	if s == nil || len(s.Messages) == 0 {
		return 0, 0
	}

	tx, err := db.BeginTx()
	if err != nil {
		indexErrors++
		return 0, 0
	}
	defer func() { _ = tx.Rollback() }()

	if err := db.TxDeleteSessionMessages(tx, s.ID); err != nil {
		indexErrors++
		return 0, 0
	}

	msgCount := 0
	for _, msg := range s.Messages {
		if err := db.TxInsertMessage(tx, msg); err != nil {
			indexErrors++
			continue
		}
		msgCount++
	}
	if msgCount == 0 {
		return 0, 0
	}

	if err := db.TxInsertSession(tx, db.Session{
		ID:                s.ID,
		Project:           s.Project,
		FirstQuery:        s.FirstQuery,
		MessageCount:      msgCount,
		Tool:              tool,
		FilePath:          path,
		Model:             s.Model,
		Provider:          s.Provider,
		TotalInputTokens:  s.InputTokens,
		TotalOutputTokens: s.OutputTokens,
		CLIVersion:        s.Version,
		GitBranch:         s.GitBranch,
		WorkingDirectory:  s.Cwd,
		StartTime:         s.StartTime,
		EndTime:           s.EndTime,
	}); err != nil {
		indexErrors++
		return 0, msgCount
	}
	if err := tx.Commit(); err != nil {
		indexErrors++
		return 0, msgCount
	}
	return 1, msgCount
}

// parseClaudeSession reads one JSONL transcript and returns its session
// fields and messages. It touches no database. On a read error part way
// through the file it returns both the records parsed so far and the
// error; the session is nil only when the file cannot be opened.
//
// Each user or assistant record yields one row per content block: text
// blocks are joined into a single row with the message's role, and each
// thinking, tool_use, and tool_result block becomes its own row with that
// block type as its role. Token usage is charged to the first row of the
// record. Records of other types (summary, attachment, and so on) are
// skipped.
func parseClaudeSession(path, tool string) (*claudeSession, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	s := &claudeSession{
		ID:      strings.TrimSuffix(filepath.Base(path), ".jsonl"),
		Project: extractProjectName(path),
	}
	s.ParentID = subagentParent(path, s.ID)

	// Lines are read without a length cap: an attachment record holding a
	// pasted PDF can run to tens of megabytes, and a cap would end the
	// session there.
	handle := func(line []byte) {
		var entry map[string]interface{}
		if err := json.Unmarshal(line, &entry); err != nil {
			return
		}

		entryType, _ := entry["type"].(string)
		if entryType != "user" && entryType != "assistant" {
			return
		}

		uuid, _ := entry["uuid"].(string)
		parentUUID, _ := entry["parentUuid"].(string)
		cwd, _ := entry["cwd"].(string)
		gitBranch, _ := entry["gitBranch"].(string)
		version, _ := entry["version"].(string)

		timestamp := time.Now()
		if tsStr, ok := entry["timestamp"].(string); ok && tsStr != "" {
			if parsed, err := time.Parse(time.RFC3339, tsStr); err == nil {
				timestamp = parsed
			}
		}

		if s.Cwd == "" && cwd != "" {
			s.Cwd = cwd
		}
		if s.GitBranch == "" && gitBranch != "" {
			s.GitBranch = gitBranch
		}
		if s.Version == "" && version != "" {
			s.Version = version
		}
		if s.ParentID == "" {
			// A subagent record names its parent; a main-transcript record
			// names itself.
			if sid, _ := entry["sessionId"].(string); sid != "" && sid != s.ID {
				if _, isAgent := entry["agentId"]; isAgent {
					s.ParentID = sid
				}
			}
		}
		if s.StartTime.IsZero() {
			s.StartTime = timestamp
		}
		s.EndTime = timestamp

		role := entryType
		var model string
		var inputTokens, outputTokens int
		var rows []db.Message

		if msg, ok := entry["message"].(map[string]interface{}); ok {
			if r, _ := msg["role"].(string); r != "" {
				role = r
			}
			if m, ok := msg["model"].(string); ok && m != "" {
				model = m
				if s.Model == "" {
					s.Model = m
				}
			}
			if usage, ok := msg["usage"].(map[string]interface{}); ok {
				if v, ok := usage["input_tokens"].(float64); ok {
					inputTokens = int(v)
				}
				if v, ok := usage["output_tokens"].(float64); ok {
					outputTokens = int(v)
				}
			}
			rows = contentRows(msg["content"], role, path, s.ID)
		} else if c, ok := entry["content"].(string); ok && c != "" {
			rows = []db.Message{{Role: role, Content: c}}
		}

		if len(rows) == 0 {
			return
		}

		s.InputTokens += inputTokens
		s.OutputTokens += outputTokens

		provider := inferProviderFromModel(model)
		if s.Provider == "" && provider != "" {
			s.Provider = provider
		}

		for i := range rows {
			r := &rows[i]
			if r.Role == "user" && s.FirstQuery == "" {
				s.FirstQuery = truncate(r.Content, 200)
			}
			r.SessionID = s.ID
			r.Project = s.Project
			r.Timestamp = timestamp
			r.Tool = tool
			r.Model = model
			r.Provider = provider
			r.MessageUUID = uuid
			r.ParentUUID = parentUUID
			r.WorkingDirectory = cwd
			r.Agent = s.ParentID
			if i == 0 {
				r.InputTokens = inputTokens
				r.OutputTokens = outputTokens
			}
		}
		s.Messages = append(s.Messages, rows...)
	}

	reader := bufio.NewReaderSize(file, 1024*1024)
	var readErr error
	for {
		line, err := reader.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			handle(trimmed)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			readErr = err
			break
		}
	}
	// A subagent's rows learn the parent only once a record has named it, so
	// fill in any rows that came before.
	if s.ParentID != "" {
		for i := range s.Messages {
			s.Messages[i].Agent = s.ParentID
		}
	}
	return s, readErr
}

// contentRows turns a message's content field into rows. Text blocks merge
// into one row with the message's role; thinking, tool_use, and tool_result
// blocks each get a row whose role is the block type. Blocks with nothing
// to index (images, redacted thinking) yield no row.
func contentRows(content interface{}, role, sessionFile, sessionID string) []db.Message {
	switch c := content.(type) {
	case string:
		if c == "" {
			return nil
		}
		return []db.Message{{Role: role, Content: c}}
	case []interface{}:
		var rows []db.Message
		var text strings.Builder
		textAt := -1 // index in rows reserved for the merged text row
		for _, item := range c {
			block, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			blockType, _ := block["type"].(string)
			switch blockType {
			case "thinking":
				if t, _ := block["thinking"].(string); t != "" {
					rows = append(rows, db.Message{Role: "thinking", Content: t})
				}
			case "tool_use":
				name, _ := block["name"].(string)
				input, _ := block["input"].(map[string]interface{})
				if r := renderToolInput(name, input); r != "" {
					rows = append(rows, db.Message{Role: "tool_use", Content: r})
				}
			case "tool_result":
				if r := toolResultText(block["content"], sessionFile, sessionID); r != "" {
					rows = append(rows, db.Message{Role: "tool_result", Content: r})
				}
			default:
				// "text" blocks, and any block that carries a text field.
				if t, ok := block["text"].(string); ok && t != "" {
					if textAt < 0 {
						textAt = len(rows)
						rows = append(rows, db.Message{Role: role})
					}
					text.WriteString(t)
					text.WriteString(" ")
				}
			}
		}
		if textAt >= 0 {
			rows[textAt].Content = strings.TrimSpace(text.String())
		}
		return rows
	}
	return nil
}

// renderToolInput renders a tool_use block as searchable text: the tool
// name, then one "key: value" line per input field in key order. Values
// that are not strings are rendered as JSON.
func renderToolInput(name string, input map[string]interface{}) string {
	if name == "" && len(input) == 0 {
		return ""
	}
	keys := make([]string, 0, len(input))
	for k := range input {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(name)
	for _, k := range keys {
		b.WriteString("\n")
		b.WriteString(k)
		b.WriteString(": ")
		switch v := input[k].(type) {
		case string:
			b.WriteString(v)
		case float64:
			b.WriteString(strconv.FormatFloat(v, 'f', -1, 64))
		default:
			if j, err := json.Marshal(v); err == nil {
				b.Write(j)
			}
		}
	}
	return b.String()
}

// toolResultText extracts the text of a tool_result block. Content is a
// string or a list of blocks, of which the text ones count. When the text
// points at a spilled tool-results file that can be found beside the
// session, the file's content follows the pointer.
func toolResultText(content interface{}, sessionFile, sessionID string) string {
	var text string
	switch c := content.(type) {
	case string:
		text = c
	case []interface{}:
		var parts []string
		for _, item := range c {
			if block, ok := item.(map[string]interface{}); ok {
				if t, ok := block["text"].(string); ok && t != "" {
					parts = append(parts, t)
				}
			}
		}
		text = strings.Join(parts, "\n")
	}
	if text == "" {
		return ""
	}

	if spill := spilledResultPath(sessionFile, sessionID, text); spill != "" {
		if body, err := readCapped(spill, maxSpilledResultBytes); err == nil && len(body) > 0 {
			text = text + "\n" + string(body)
		}
	}
	return text
}

// spilledResultPath finds the tool-results file a tool_result points at, or
// returns "" when there is no pointer or no such file. The pointer's
// directory is ignored, since it names a path on the machine that wrote the
// transcript; only the base name is used, and the file is looked for
// beside the session:
//
//	<dir>/<sessionID>/tool-results/<name>   for a main transcript
//	<dir>/../tool-results/<name>            for a subagent transcript
//
// The literal pointer path is tried last.
func spilledResultPath(sessionFile, sessionID, text string) string {
	m := spilledResultRe.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	pointer := m[1]
	// Base name for either path flavor.
	base := pointer
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	if base == "" {
		return ""
	}

	dir := filepath.Dir(sessionFile)
	candidates := []string{
		filepath.Join(dir, sessionID, "tool-results", base),
		filepath.Join(dir, "..", "tool-results", base),
		pointer,
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c
		}
	}
	return ""
}

// readCapped reads at most limit bytes of a file.
func readCapped(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, limit))
}

// subagentParent returns the parent session ID when path is a subagent
// transcript (<parent>/subagents/<id>.jsonl), else "".
func subagentParent(path, sessionID string) string {
	dir := filepath.Dir(path)
	if filepath.Base(dir) != "subagents" {
		return ""
	}
	parent := filepath.Base(filepath.Dir(dir))
	if parent == "" || parent == "." || parent == sessionID {
		return ""
	}
	return parent
}
