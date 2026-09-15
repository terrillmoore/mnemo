// version.go prints build-time version information.
package cmd

import (
	"fmt"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"
)

// Build-time variables. goreleaser sets all three with -ldflags. A plain
// go build or go install leaves them, and init then fills in what the Go
// toolchain records in every binary built from a git checkout: the module
// version, the commit, and the commit time.
var (
	Version   = "dev"
	BuildDate = "unknown"
	GitCommit = "unknown"
)

// dateLabel names what BuildDate holds. goreleaser supplies the time of the
// build; the VCS stamp supplies the time of the commit, which is earlier, and
// says nothing about uncommitted changes (those show as -dirty on the commit).
var dateLabel = "Build date"

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("mnemo %s\n", Version)
		fmt.Printf("%s: %s\n", dateLabel, BuildDate)
		fmt.Printf("Git commit: %s\n", GitCommit)
	},
}

// applyBuildInfo fills any of Version, BuildDate, and GitCommit that -ldflags
// left at its default from the binary's embedded build information.
func applyBuildInfo(info *debug.BuildInfo) {
	if info == nil {
		return
	}
	var revision, commitTime string
	modified := false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.time":
			commitTime = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	// Main.Version is a tag only for a module fetched by version, as with
	// go install github.com/...@v1.3.8; a build from a checkout says (devel).
	if v := strings.TrimPrefix(info.Main.Version, "v"); Version == "dev" && v != "" && v != "(devel)" {
		Version = v
	}
	if GitCommit == "unknown" && revision != "" {
		if len(revision) > 7 {
			revision = revision[:7]
		}
		if modified {
			revision += "-dirty"
		}
		GitCommit = revision
	}
	if BuildDate == "unknown" && commitTime != "" {
		BuildDate = commitTime
		dateLabel = "Commit date"
	}
}

func init() {
	if info, ok := debug.ReadBuildInfo(); ok {
		applyBuildInfo(info)
	}
	rootCmd.AddCommand(versionCmd)
}
