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
	s := server.NewMCPServer(
		"mnemo",
		Version,
		server.WithToolCapabilities(false),
		server.WithRecovery(),
	)

	// Read-only: this server answers queries and must never write to the
	// database it is pointed at. On an archive host it runs under a forced
	// ssh command for remote endpoints, where a schema migration triggered by
	// a client would be plainly wrong.
	if err := db.InitReadOnly(); err != nil {
		return fmt.Errorf("failed to initialize database: %w", err)
	}
	defer db.CloseDB()

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
			mcp.Description("Filter results by project name (optional)"),
		),
	)

	s.AddTool(searchTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, err := request.RequireString("query")
		if err != nil {
			return mcp.NewToolResultError("query parameter is required"), nil
		}

		limit := request.GetInt("limit", 5)
		projectFilter := request.GetString("project", "")

		// When filtering by project, search with a higher limit to avoid missing
		// relevant results that would be pushed beyond the limit threshold.
		searchLimit := limit
		if projectFilter != "" {
			searchLimit = limit * 5
		}

		results, err := db.SearchGrouped(query, searchLimit)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("search failed: %v", err)), nil
		}

		if projectFilter != "" {
			filtered := make([]db.SessionMatch, 0, limit)
			for _, r := range results {
				if strings.EqualFold(r.Project, projectFilter) {
					filtered = append(filtered, r)
				}
			}
			results = filtered
			if len(results) > limit {
				results = results[:limit]
			}
		}

		return mcp.NewToolResultJSON(searchResponse{
			Query:   query,
			Project: optString(projectFilter),
			Count:   len(results),
			Results: newSessionHits(results),
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

	log.Printf("mnemo MCP server starting...")

	if err := server.ServeStdio(s); err != nil {
		return fmt.Errorf("server error: %w", err)
	}

	return nil
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
