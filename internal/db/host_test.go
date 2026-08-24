package db

import (
	"path/filepath"
	"testing"
)

func sessionHost(t *testing.T, id string) string {
	t.Helper()
	var h string
	if err := db.QueryRow("SELECT host FROM sessions WHERE id = ?", id).Scan(&h); err != nil {
		t.Fatalf("reading host for %s: %v", id, err)
	}
	return h
}

func TestInsertSessionRecordsDefaultHost(t *testing.T) {
	defer setupTestDB(t)()

	saved := Host()
	defer SetHost(saved)
	SetHost("apollo")

	if err := InsertSession(Session{ID: "s1", Project: "p", Tool: "claude"}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	if got := sessionHost(t, "s1"); got != "apollo" {
		t.Errorf("host = %q, want %q", got, "apollo")
	}
}

// An explicit Host on the Session wins, so one process can index several
// machines' transcripts in turn.
func TestInsertSessionHostFieldOverridesDefault(t *testing.T) {
	defer setupTestDB(t)()

	saved := Host()
	defer SetHost(saved)
	SetHost("apollo")

	if err := InsertSession(Session{ID: "s1", Project: "p", Host: "winbox"}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	if got := sessionHost(t, "s1"); got != "winbox" {
		t.Errorf("host = %q, want %q", got, "winbox")
	}
}

func TestInsertSessionSimpleRecordsHost(t *testing.T) {
	defer setupTestDB(t)()

	saved := Host()
	defer SetHost(saved)
	SetHost("apollo")

	if err := InsertSessionSimple("s1", "p", "q", "/tmp/x", "claude", 1); err != nil {
		t.Fatalf("InsertSessionSimple: %v", err)
	}
	if got := sessionHost(t, "s1"); got != "apollo" {
		t.Errorf("host = %q, want %q", got, "apollo")
	}
}

// ClaimEmptyHosts must not relabel sessions that already record where they
// came from: on an archive holding several machines, that would be silent
// corruption.
func TestClaimEmptyHostsLeavesClaimedSessionsAlone(t *testing.T) {
	defer setupTestDB(t)()

	if err := InsertSession(Session{ID: "claimed", Project: "p", Host: "winbox"}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	if _, err := db.Exec(
		"INSERT INTO sessions (id, project, first_query, message_count, tool, file_path) VALUES ('unclaimed','p','q',1,'claude','/tmp/y')",
	); err != nil {
		t.Fatalf("inserting a host-less row: %v", err)
	}

	n, err := ClaimEmptyHosts("apollo")
	if err != nil {
		t.Fatalf("ClaimEmptyHosts: %v", err)
	}
	if n != 1 {
		t.Errorf("claimed %d rows, want 1", n)
	}
	if got := sessionHost(t, "unclaimed"); got != "apollo" {
		t.Errorf("unclaimed host = %q, want %q", got, "apollo")
	}
	if got := sessionHost(t, "claimed"); got != "winbox" {
		t.Errorf("claimed host = %q, want it left as %q", got, "winbox")
	}

	// Running it again finds nothing left to do.
	n, err = ClaimEmptyHosts("apollo")
	if err != nil {
		t.Fatalf("second ClaimEmptyHosts: %v", err)
	}
	if n != 0 {
		t.Errorf("second run claimed %d rows, want 0", n)
	}
}

func TestSetAllHostsOverwrites(t *testing.T) {
	defer setupTestDB(t)()

	if err := InsertSession(Session{ID: "a", Project: "p", Host: "winbox"}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	if err := InsertSession(Session{ID: "b", Project: "p", Host: "apollo"}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	n, err := SetAllHosts("archive")
	if err != nil {
		t.Fatalf("SetAllHosts: %v", err)
	}
	if n != 2 {
		t.Errorf("changed %d rows, want 2", n)
	}
	for _, id := range []string{"a", "b"} {
		if got := sessionHost(t, id); got != "archive" {
			t.Errorf("host of %s = %q, want %q", id, got, "archive")
		}
	}
}

func TestHostCounts(t *testing.T) {
	defer setupTestDB(t)()

	for id, h := range map[string]string{"a": "winbox", "b": "apollo", "c": "apollo"} {
		if err := InsertSession(Session{ID: id, Project: "p", Host: h}); err != nil {
			t.Fatalf("InsertSession %s: %v", id, err)
		}
	}

	counts, err := HostCounts()
	if err != nil {
		t.Fatalf("HostCounts: %v", err)
	}
	if counts["apollo"] != 2 {
		t.Errorf("apollo = %d, want 2", counts["apollo"])
	}
	if counts["winbox"] != 1 {
		t.Errorf("winbox = %d, want 1", counts["winbox"])
	}
}

// initInTempHome points InitDB at a throwaway home so migrations run for real.
func initInTempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return filepath.Join(home, ".mnemo", "mnemo.db")
}

// A database created before the host column existed has every session claimed
// for the local machine the first time the fork opens it, because those
// sessions can only have come from there. Re-indexing cannot recover them:
// sessions whose transcripts have been deleted exist only in the database.
func TestFirstMigrationClaimsExistingSessions(t *testing.T) {
	initInTempHome(t)

	saved := Host()
	defer SetHost(saved)
	SetHost("apollo")

	// Build a database as an upstream build would leave it: upstream's schema
	// and migrations, no fork migrations, so no host column and no
	// fork_schema. The column in the CREATE TABLE only takes effect on a file
	// that does not exist yet, so drop it back off afterwards.
	realFork := forkMigrations
	forkMigrations = nil
	if err := InitDB(); err != nil {
		forkMigrations = realFork
		t.Fatalf("building the upstream database: %v", err)
	}
	if _, err := db.Exec("ALTER TABLE sessions DROP COLUMN host"); err != nil {
		forkMigrations = realFork
		CloseDB()
		t.Fatalf("removing the host column: %v", err)
	}
	if _, err := db.Exec("INSERT INTO sessions (id, project) VALUES ('old1','p'), ('old2','p')"); err != nil {
		forkMigrations = realFork
		CloseDB()
		t.Fatalf("inserting pre-fork sessions: %v", err)
	}
	CloseDB()
	forkMigrations = realFork

	if err := InitDB(); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer CloseDB()

	counts, err := HostCounts()
	if err != nil {
		t.Fatalf("HostCounts: %v", err)
	}
	if counts["apollo"] != 2 {
		t.Errorf("apollo = %d, want 2 (existing sessions should be claimed)", counts["apollo"])
	}

	_, fork, name, err := SchemaVersions()
	if err != nil {
		t.Fatalf("SchemaVersions: %v", err)
	}
	if fork != len(forkMigrations) {
		t.Errorf("fork_schema = %d, want %d", fork, len(forkMigrations))
	}
	if name != ForkName {
		t.Errorf("fork_name = %q, want %q", name, ForkName)
	}
}

// After the first migration, a host-less row is left alone. Such a row means
// some other build wrote it, and this machine cannot know where it came from.
func TestLaterOpensDoNotClaimHostlessRows(t *testing.T) {
	initInTempHome(t)

	saved := Host()
	defer SetHost(saved)
	SetHost("apollo")

	if err := InitDB(); err != nil {
		t.Fatalf("first InitDB: %v", err)
	}
	if _, err := db.Exec(
		"INSERT INTO sessions (id, project, host) VALUES ('stock','p','')",
	); err != nil {
		t.Fatalf("inserting a host-less row: %v", err)
	}
	CloseDB()

	if err := InitDB(); err != nil {
		t.Fatalf("second InitDB: %v", err)
	}
	defer CloseDB()

	counts, err := HostCounts()
	if err != nil {
		t.Fatalf("HostCounts: %v", err)
	}
	if counts[""] != 1 {
		t.Errorf("unclaimed = %d, want 1 (a later open must not claim it)", counts[""])
	}
}

// Upstream stamps no version, so it cannot tell an older binary to stop. The
// fork can, and must: an old build would misread rows a newer one wrote.
func TestNewerForkSchemaIsRefused(t *testing.T) {
	initInTempHome(t)

	if err := InitDB(); err != nil {
		t.Fatalf("first InitDB: %v", err)
	}
	if err := setSchemaMetaInt("fork_schema", len(forkMigrations)+1); err != nil {
		t.Fatalf("setSchemaMetaInt: %v", err)
	}
	CloseDB()

	err := InitDB()
	if err == nil {
		CloseDB()
		t.Fatal("InitDB accepted a database from a newer fork build; want an error")
	}
}
