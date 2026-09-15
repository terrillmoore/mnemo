package cmd

import (
	"runtime/debug"
	"testing"
)

func saveVersionVars(t *testing.T) {
	t.Helper()
	v, d, c, l := Version, BuildDate, GitCommit, dateLabel
	t.Cleanup(func() { Version, BuildDate, GitCommit, dateLabel = v, d, c, l })
}

func vcsInfo(version, revision, when string, modified bool) *debug.BuildInfo {
	info := &debug.BuildInfo{}
	info.Main.Version = version
	info.Settings = []debug.BuildSetting{
		{Key: "vcs.revision", Value: revision},
		{Key: "vcs.time", Value: when},
		{Key: "vcs.modified", Value: map[bool]string{true: "true", false: "false"}[modified]},
	}
	return info
}

// A plain go build from a checkout reports the commit and its time, and says
// so, rather than a version and date left over from some earlier release.
func TestApplyBuildInfoFromCheckout(t *testing.T) {
	saveVersionVars(t)
	Version, BuildDate, GitCommit, dateLabel = "dev", "unknown", "unknown", "Build date"

	applyBuildInfo(vcsInfo("(devel)", "0a854271234567890abcdef", "2026-09-15T15:02:03Z", true))

	if Version != "dev" {
		t.Errorf("Version = %q, want dev for a (devel) build", Version)
	}
	if GitCommit != "0a85427-dirty" {
		t.Errorf("GitCommit = %q, want 0a85427-dirty", GitCommit)
	}
	if BuildDate != "2026-09-15T15:02:03Z" || dateLabel != "Commit date" {
		t.Errorf("BuildDate, dateLabel = %q, %q; want the commit time labelled as such", BuildDate, dateLabel)
	}
}

func TestApplyBuildInfoModuleVersion(t *testing.T) {
	saveVersionVars(t)
	Version, BuildDate, GitCommit, dateLabel = "dev", "unknown", "unknown", "Build date"

	applyBuildInfo(vcsInfo("v1.3.8", "abcdef0", "2026-05-22T20:44:52Z", false))

	if Version != "1.3.8" || GitCommit != "abcdef0" {
		t.Errorf("Version, GitCommit = %q, %q; want 1.3.8, abcdef0", Version, GitCommit)
	}
}

// -ldflags values win: goreleaser knows the release version and build time,
// and the VCS stamp must not overwrite either.
func TestApplyBuildInfoKeepsLdflags(t *testing.T) {
	saveVersionVars(t)
	Version, BuildDate, GitCommit, dateLabel = "1.4.0", "2026-10-01T00:00:00Z", "1234567", "Build date"

	applyBuildInfo(vcsInfo("(devel)", "abcdef0abcdef0", "2026-09-15T15:02:03Z", true))

	if Version != "1.4.0" || BuildDate != "2026-10-01T00:00:00Z" || GitCommit != "1234567" || dateLabel != "Build date" {
		t.Errorf("ldflags values changed: %q %q %q %q", Version, BuildDate, GitCommit, dateLabel)
	}
}

func TestApplyBuildInfoNil(t *testing.T) {
	saveVersionVars(t)
	applyBuildInfo(nil)
}
