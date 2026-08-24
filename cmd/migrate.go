// migrate.go carries the fork's one-off database maintenance commands: the
// host attribution that `mnemo index --host` sets going forward, applied to
// rows already in the database, and a report of the schema versions.
package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/Pilan-AI/mnemo/internal/db"
	"github.com/spf13/cobra"
)

var (
	migrateHostSet string
	migrateHostAll bool
)

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Database maintenance: host attribution and schema version reporting",
}

var migrateHostCmd = &cobra.Command{
	Use:   "host",
	Short: "Attribute sessions to a machine",
	Long: `Attribute sessions already in the database to a machine.

Sessions indexed before the host column existed, or by an upstream mnemo build
that does not know about it, carry no host. An empty host means the origin was
never recorded, not that the session came from nowhere, so claiming those rows
states where the database has lived.

By default only unclaimed sessions are changed, which makes the command safe to
re-run. --all overwrites hosts already recorded; use it only when a database has
been mislabelled, since it cannot tell one machine's sessions from another's.

With no --set, reports how sessions are attributed and changes nothing.`,
	Run: func(cmd *cobra.Command, args []string) {
		if !openExistingDB() {
			return
		}
		defer db.CloseDB()

		if migrateHostSet == "" {
			reportHosts()
			return
		}

		var (
			n   int64
			err error
		)
		if migrateHostAll {
			n, err = db.SetAllHosts(migrateHostSet)
		} else {
			n, err = db.ClaimEmptyHosts(migrateHostSet)
		}
		if err != nil {
			fmt.Printf("  Error: %v\n", err)
			os.Exit(1)
		}

		what := "unclaimed session"
		if migrateHostAll {
			what = "session"
		}
		fmt.Printf("  %d %s(s) now attributed to %q\n\n", n, what, migrateHostSet)
		reportHosts()
	},
}

var migrateStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the database schema versions and host attribution",
	Run: func(cmd *cobra.Command, args []string) {
		if !openExistingDB() {
			return
		}
		defer db.CloseDB()

		upstream, fork, name, err := db.SchemaVersions()
		if err != nil {
			fmt.Printf("  Error: %v\n", err)
			os.Exit(1)
		}
		if name == "" {
			name = "(none recorded)"
		}
		fmt.Printf("  Fork:             %s\n", name)
		fmt.Printf("  Fork schema:      %d\n", fork)
		fmt.Printf("  Upstream schema:  %d\n\n", upstream)
		reportHosts()
	},
}

// openExistingDB opens the database without creating one, so a report command
// never leaves an empty database behind. It reports whether it succeeded.
func openExistingDB() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Printf("  Error: cannot determine home directory: %v\n", err)
		return false
	}
	if !pathExists(filepath.Join(home, ".mnemo", "mnemo.db")) {
		fmt.Println("  No database found. Run 'mnemo index' first.")
		return false
	}
	if err := db.InitDB(); err != nil {
		fmt.Printf("  Error opening database: %v\n", err)
		return false
	}
	return true
}

func reportHosts() {
	counts, err := db.HostCounts()
	if err != nil {
		fmt.Printf("  Error: %v\n", err)
		return
	}
	if len(counts) == 0 {
		fmt.Println("  No sessions indexed.")
		return
	}

	hosts := make([]string, 0, len(counts))
	for h := range counts {
		hosts = append(hosts, h)
	}
	sort.Slice(hosts, func(i, j int) bool {
		if counts[hosts[i]] != counts[hosts[j]] {
			return counts[hosts[i]] > counts[hosts[j]]
		}
		return hosts[i] < hosts[j]
	})

	fmt.Println("  Sessions by host:")
	for _, h := range hosts {
		label := h
		if label == "" {
			label = "(unclaimed)"
		}
		fmt.Printf("    %6d  %s\n", counts[h], label)
	}
	if counts[""] > 0 {
		fmt.Printf("\n  Claim the unclaimed with: mnemo migrate host --set %s\n", db.Host())
	}
}

func init() {
	migrateHostCmd.Flags().StringVar(&migrateHostSet, "set", "", "Machine to attribute sessions to")
	migrateHostCmd.Flags().BoolVar(&migrateHostAll, "all", false, "Also overwrite sessions that already record a host")
	migrateCmd.AddCommand(migrateHostCmd)
	migrateCmd.AddCommand(migrateStatusCmd)
	rootCmd.AddCommand(migrateCmd)
}
