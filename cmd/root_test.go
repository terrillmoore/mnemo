package cmd

import (
	"testing"

	"github.com/spf13/cobra"
)

// newTestRoot builds a root with the same persistent flag the real root
// declares, plus one child per name, without touching the package rootCmd.
func newTestRoot(names ...string) *cobra.Command {
	root := &cobra.Command{Use: "mnemo"}
	root.PersistentFlags().Bool("non-interactive", false, "")
	for _, n := range names {
		root.AddCommand(&cobra.Command{Use: n, Run: func(*cobra.Command, []string) {}})
	}
	return root
}

func findCmd(t *testing.T, root *cobra.Command, name string, flags ...string) *cobra.Command {
	t.Helper()
	c, _, err := root.Find([]string{name})
	if err != nil {
		t.Fatalf("Find(%q): %v", name, err)
	}
	if err := c.ParseFlags(flags); err != nil {
		t.Fatalf("ParseFlags(%q, %v): %v", name, flags, err)
	}
	return c
}

// index on an empty database must index, not onboard: onboarding sets a
// one-month cutoff, and the archive indexer's first run lost every older
// session to it.
func TestSkipOnboardingByCommand(t *testing.T) {
	skip := []string{"onboarding", "version", "help", "completion", "status", "inject", "serve", "migrate", "host", "index"}
	run := []string{"search", "recent", "context", "blocks", "projects", "add", "tools"}
	root := newTestRoot(append(append([]string{}, skip...), run...)...)

	for _, name := range skip {
		if !skipOnboarding(findCmd(t, root, name)) {
			t.Errorf("skipOnboarding(%q) = false, want true", name)
		}
	}
	for _, name := range run {
		if skipOnboarding(findCmd(t, root, name)) {
			t.Errorf("skipOnboarding(%q) = true, want false", name)
		}
	}
}

func TestSkipOnboardingNonInteractive(t *testing.T) {
	// A fresh root per case: ParseFlags leaves the flag set on the command.
	if !skipOnboarding(findCmd(t, newTestRoot("search"), "search", "--non-interactive")) {
		t.Error("skipOnboarding(search --non-interactive) = false, want true")
	}
	if skipOnboarding(findCmd(t, newTestRoot("search"), "search")) {
		t.Error("skipOnboarding(search) = true, want false")
	}
}

// The real command tree must agree with the test double: index is a
// registered command and the flag exists on the root.
func TestSkipOnboardingRealIndexCmd(t *testing.T) {
	c, _, err := rootCmd.Find([]string{"index"})
	if err != nil || c.Name() != "index" {
		t.Fatalf("rootCmd.Find(index) = %v, %v", c, err)
	}
	if !skipOnboarding(c) {
		t.Error("skipOnboarding(rootCmd index) = false, want true")
	}
	if rootCmd.PersistentFlags().Lookup("non-interactive") == nil {
		t.Error("rootCmd lacks the --non-interactive persistent flag")
	}
}
