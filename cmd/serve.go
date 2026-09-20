package cmd

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/Pilan-AI/mnemo/internal/db"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start mnemo MCP server (used internally by MCP clients)",
	Long: `Start mnemo as an MCP (Model Context Protocol) server over stdio.

This command is launched automatically by MCP clients like Claude Desktop
and Cursor. You do not need to run it manually.

To set up MCP integration, run:
  mnemo install

This writes the config that tells your MCP client to launch mnemo serve
on demand. Restart your MCP client after installing.`,
	Run: func(cmd *cobra.Command, args []string) {
		if err := serveMCP(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	},
}

func init() {
	rootCmd.AddCommand(serveCmd)
}

// serveMCP starts an MCP (Model Context Protocol) server over stdio, exposing
// mnemo's search, context, recent, and tools capabilities to MCP clients like
// Claude Desktop and Claude Code.
func serveMCP() error {
	// Read-only: this server answers queries and must never write to the
	// database it is pointed at. On an archive host it runs under a forced
	// ssh command for remote endpoints, where a schema migration triggered by
	// a client would be plainly wrong.
	if err := db.InitReadOnly(); err != nil {
		return fmt.Errorf("failed to initialize database: %w", err)
	}
	defer db.CloseDB()

	s := newMCPServer()

	log.Printf("mnemo MCP server starting...")

	if err := server.ServeStdio(s); err != nil {
		return fmt.Errorf("server error: %w", err)
	}

	return nil
}

// newMCPServer builds the server and registers the tools, against whatever
// database the package is already connected to. It is separate from serveMCP
// so a test can call the same handlers a client reaches, through
// MCPServer.HandleMessage, without a database of its own choosing, a process,
// or stdio.
func newMCPServer() *server.MCPServer {
	s := server.NewMCPServer(
		"mnemo",
		Version,
		server.WithToolCapabilities(false),
		server.WithRecovery(),
	)

	searchTool := mcp.NewTool("mnemo_search",
		mcp.WithDescription("Search across all indexed AI coding conversations"),
		mcp.WithOutputSchema[searchResponse](),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("Search query - can be keywords, code patterns, or natural language"),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of results to return (default: 10)"),
		),
		mcp.WithString("project",
			mcp.Description("Only sessions whose project name contains this text, ignoring case. The project name is derived by each adapter with a heuristic; host and working_directory are dependable, this is not."),
		),
		mcp.WithString("host",
			mcp.Description("Only sessions from this machine, named exactly, for an index holding several machines' history"),
		),
		mcp.WithString("working_directory",
			mcp.Description("Only sessions whose working directory contains this text, so a fragment of a long path will do"),
		),
		mcp.WithArray("role",
			mcp.Description("Only hits in these block types: user, assistant, tool_use, tool_result or thinking. A tool_use hit is a command someone ran, not a conclusion anyone reached."),
			mcp.WithStringItems(),
		),
		mcp.WithString("since",
			mcp.Description("Only sessions that started on or after this date, written YYYY-MM-DD"),
		),
	)

	s.AddTool(searchTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, err := request.RequireString("query")
		if err != nil {
			return mcp.NewToolResultError("query parameter is required"), nil
		}

		limit := request.GetInt("limit", 5)

		// Every filter is applied in SQL. The project filter used to run in
		// Go after the query, as an exact case-insensitive match on a name
		// the adapters derive inconsistently, so a near miss returned
		// nothing and nothing said the filter had done it.
		filter := db.SearchFilter{
			Host:             request.GetString("host", ""),
			WorkingDirectory: request.GetString("working_directory", ""),
			Project:          request.GetString("project", ""),
			Roles:            request.GetStringSlice("role", nil),
			Since:            request.GetString("since", ""),
		}

		found, err := db.SearchGroupedExplained(query, limit, filter)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("search failed: %v", err)), nil
		}
		results := found.Matches

		missing := found.TermsWithoutMatches
		if missing == nil {
			missing = []string{}
		}

		return mcp.NewToolResultJSON(searchResponse{
			Query:               query,
			Filters:             newSearchFilters(filter),
			Mode:                string(found.Mode),
			TermsWithoutMatches: missing,
			Count:               len(results),
			Results:             newSessionHits(results),
		})
	})

	contextTool := mcp.NewTool("mnemo_context",
		mcp.WithDescription("Generate context summary for a project based on past sessions"),
		mcp.WithOutputSchema[contextResponse](),
		mcp.WithString("project",
			mcp.Required(),
			mcp.Description("Project name to get context for"),
		),
	)

	s.AddTool(contextTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		project, err := request.RequireString("project")
		if err != nil {
			return mcp.NewToolResultError("project parameter is required"), nil
		}

		results, err := db.SearchGrouped(project, 10)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("context search failed: %v", err)), nil
		}

		// Filter to matching project
		filtered := make([]db.SessionMatch, 0)
		for _, r := range results {
			if strings.EqualFold(r.Project, project) || strings.Contains(strings.ToLower(r.Project), strings.ToLower(project)) {
				filtered = append(filtered, r)
			}
		}
		if len(filtered) > 0 {
			results = filtered
		}

		return mcp.NewToolResultJSON(contextResponse{
			Project:  project,
			Count:    len(results),
			Sessions: newSessionHits(results),
		})
	})

	// Search says which session answers a question; this reads the answer.
	// It matters most for a caller that holds no transcripts: the archive is
	// the durable copy of sessions whose files were deleted long ago.
	sessionTool := mcp.NewTool("mnemo_session",
		mcp.WithDescription("Read the messages of one past session, by the session_id a search returned"),
		mcp.WithOutputSchema[sessionResponse](),
		mcp.WithString("session_id",
			mcp.Required(),
			mcp.Description("The session to read, as mnemo_search reports it"),
		),
		mcp.WithArray("roles",
			mcp.Description("Which block types to return; the conversation (user, assistant) by default. The others are tool_use, tool_result and thinking, and a full transcript with tool results is very large."),
			mcp.WithStringItems(),
		),
		mcp.WithNumber("limit",
			mcp.Description("Rows to return (default: 50)"),
		),
		mcp.WithNumber("offset",
			mcp.Description("Rows to skip, for reading a long session a page at a time (default: 0)"),
		),
	)

	s.AddTool(sessionTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sessionID, err := request.RequireString("session_id")
		if err != nil {
			return mcp.NewToolResultError("session_id parameter is required"), nil
		}

		roles := request.GetStringSlice("roles", []string{"user", "assistant"})
		limit := request.GetInt("limit", 50)
		offset := request.GetInt("offset", 0)

		session, found, err := db.GetSession(sessionID)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("reading the session failed: %v", err)), nil
		}
		if !found {
			// A miss is worth distinguishing from an empty session: the id
			// may be from another machine's index, or from a database this
			// server is not pointed at.
			return mcp.NewToolResultError(fmt.Sprintf("no session %q in this index", sessionID)), nil
		}

		messages, total, err := db.GetSessionMessages(sessionID, roles, limit, offset)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("reading the session's messages failed: %v", err)), nil
		}

		return mcp.NewToolResultJSON(newSessionResponse(session, roles, total, offset, limit, messages))
	})

	recentTool := mcp.NewTool("mnemo_recent",
		mcp.WithDescription("Show recent AI coding sessions"),
		mcp.WithOutputSchema[recentResponse](),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of sessions to return (default: 10)"),
		),
	)

	s.AddTool(recentTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		limit := request.GetInt("limit", 10)

		sessions, err := db.GetRecentSessions(limit)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to get recent sessions: %v", err)), nil
		}

		return mcp.NewToolResultJSON(recentResponse{
			Limit:    limit,
			Count:    len(sessions),
			Sessions: newRecentEntries(sessions),
		})
	})

	toolsTool := mcp.NewTool("mnemo_tools",
		mcp.WithDescription("List detected AI coding tools on this system"),
		mcp.WithOutputSchema[toolsResponse](),
	)

	s.AddTool(toolsTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultJSON(newToolsResponse(detectTools()))
	})

	return s
}

// Tool represents a detected AI coding tool for the MCP tools endpoint.
type Tool struct {
	Name      string
	Path      string
	Installed bool
}

// detectTools checks for known AI coding tool installations on the local system.
func detectTools() []Tool {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	tools := []Tool{
		{Name: "Claude Code", Path: filepath.Join(home, ".claude", "projects")},
		{Name: "Opencode", Path: filepath.Join(home, ".local", "share", "opencode")},
		{Name: "Cursor", Path: filepath.Join(appSupportDir(home, "Cursor"), "User", "globalStorage")},
		{Name: "Gemini CLI", Path: filepath.Join(home, ".gemini")},
		{Name: "Windsurf", Path: appSupportDir(home, "Windsurf")},
		{Name: "Aider", Path: filepath.Join(home, ".aider.chat.history.md")},
		{Name: "GitHub Copilot", Path: filepath.Join(home, ".config", "github-copilot")},
		{Name: "Amp", Path: filepath.Join(home, ".amp")},
	}

	for i := range tools {
		if _, err := os.Stat(tools[i].Path); err == nil {
			tools[i].Installed = true
		}
	}

	return tools
}
