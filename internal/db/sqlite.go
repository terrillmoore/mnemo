// Package db provides the SQLite persistence layer for mnemo.
//
// All indexed sessions and messages are stored in ~/.mnemo/mnemo.db using
// pure-Go SQLite (modernc.org/sqlite, no CGO required). The schema includes:
//
//   - messages table with FTS5 virtual table for full-text search
//   - sessions table linking messages to projects and tools
//   - token_usage table for per-request cost tracking
//   - projects table for directory-based project management
//
// FTS5 triggers automatically keep the search index in sync with inserts,
// updates, and deletes on the messages table.
package db

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

var db *sql.DB

// Path reports the database file mnemo uses: $MNEMO_DB when set, otherwise
// ~/.mnemo/mnemo.db.
//
// The variable exists so one machine can hold more than one index. An endpoint
// of a multi-machine archive keeps its own live index at the default path and
// a read-only copy of the merged archive beside it, and searches either by
// setting MNEMO_DB. Without it the path is fixed and the second index is
// unreachable.
func Path() (string, error) {
	if p := os.Getenv("MNEMO_DB"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(home, ".mnemo", "mnemo.db"), nil
}

// GetDB returns the package-level database connection.
// Must call InitDB first.
func GetDB() *sql.DB {
	return db
}

// InitDB opens (or creates) the mnemo database at ~/.mnemo/mnemo.db and
// applies the schema and any pending migrations. Uses WAL mode for
// concurrent read access.
func InitDB() error {
	dbPath, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
		return fmt.Errorf("failed to create mnemo directory: %w", err)
	}

	db, err = sql.Open("sqlite", dbPath+"?_synchronous=NORMAL&_journal_mode=WAL&_cache_size=-10000&_temp_store=MEMORY&_busy_timeout=5000")
	if err != nil {
		db = nil
		return fmt.Errorf("failed to open database: %w", err)
	}
	// Every failure from here on must release the handle. Callers do not call
	// CloseDB after an InitDB error, and an open handle keeps the file busy:
	// on Windows that blocks deleting it, which is how a refused database
	// broke the test suite's temporary-directory cleanup.
	opened := false
	defer func() {
		if !opened {
			_ = db.Close()
			db = nil
		}
	}()

	// SQLite is single-writer; limit connections to prevent "database is locked" errors
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}

	// Verify database integrity on open
	var integrityResult string
	if err := db.QueryRow("PRAGMA integrity_check(1)").Scan(&integrityResult); err == nil && integrityResult != "ok" {
		log.Printf("warning: database integrity check failed: %s", integrityResult)
	}

	// Set file permissions: owner read/write only
	if err := os.Chmod(dbPath, 0600); err != nil {
		log.Printf("warning: failed to set database permissions: %v", err)
	}

	schema := `
	CREATE TABLE IF NOT EXISTS messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL,
		project TEXT NOT NULL,
		role TEXT NOT NULL,
		content TEXT NOT NULL,
		timestamp DATETIME,
		tool TEXT DEFAULT 'claude',
		model TEXT DEFAULT '',
		provider TEXT DEFAULT '',
		input_tokens INTEGER DEFAULT 0,
		output_tokens INTEGER DEFAULT 0,
		cache_read_tokens INTEGER DEFAULT 0,
		cache_write_tokens INTEGER DEFAULT 0,
		cost_usd REAL DEFAULT 0.0,
		message_uuid TEXT DEFAULT '',
		parent_uuid TEXT DEFAULT '',
		working_directory TEXT DEFAULT ''
	);

	CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
		content,
		project,
		session_id,
		content='messages',
		content_rowid='id'
	);

	CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
		INSERT INTO messages_fts(rowid, content, project, session_id)
		VALUES (new.id, new.content, new.project, new.session_id);
	END;

	CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
		INSERT INTO messages_fts(messages_fts, rowid, content, project, session_id)
		VALUES ('delete', old.id, old.content, old.project, old.session_id);
	END;

	CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE ON messages BEGIN
		INSERT INTO messages_fts(messages_fts, rowid, content, project, session_id)
		VALUES ('delete', old.id, old.content, old.project, old.session_id);
		INSERT INTO messages_fts(rowid, content, project, session_id)
		VALUES (new.id, new.content, new.project, new.session_id);
	END;

	CREATE TABLE IF NOT EXISTS sessions (
		id TEXT PRIMARY KEY,
		project TEXT NOT NULL,
		first_query TEXT,
		message_count INTEGER DEFAULT 0,
		tool TEXT DEFAULT 'claude',
		file_path TEXT,
		indexed_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		model TEXT DEFAULT '',
		provider TEXT DEFAULT '',
		total_input_tokens INTEGER DEFAULT 0,
		total_output_tokens INTEGER DEFAULT 0,
		total_cache_read INTEGER DEFAULT 0,
		total_cache_write INTEGER DEFAULT 0,
		total_cost_usd REAL DEFAULT 0.0,
		cli_version TEXT DEFAULT '',
		git_branch TEXT DEFAULT '',
		working_directory TEXT DEFAULT '',
		start_time DATETIME,
		end_time DATETIME,
		host TEXT NOT NULL DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS token_usage (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL,
		timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
		model TEXT NOT NULL,
		input_tokens INTEGER DEFAULT 0,
		output_tokens INTEGER DEFAULT 0,
		cache_read_tokens INTEGER DEFAULT 0,
		cache_write_tokens INTEGER DEFAULT 0,
		total_tokens INTEGER DEFAULT 0,
		cost_usd REAL DEFAULT 0.0,
		provider TEXT DEFAULT 'anthropic',
		FOREIGN KEY (session_id) REFERENCES sessions(id)
	);

	CREATE TABLE IF NOT EXISTS api_credentials (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		provider TEXT NOT NULL UNIQUE,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		last_used DATETIME,
		is_valid INTEGER DEFAULT 1
	);

	CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id);
	CREATE INDEX IF NOT EXISTS idx_messages_project ON messages(project);
	CREATE INDEX IF NOT EXISTS idx_messages_timestamp ON messages(timestamp);
	CREATE INDEX IF NOT EXISTS idx_messages_model ON messages(model);
	CREATE INDEX IF NOT EXISTS idx_sessions_project ON sessions(project);
	CREATE INDEX IF NOT EXISTS idx_sessions_tool ON sessions(tool);
	CREATE INDEX IF NOT EXISTS idx_sessions_model ON sessions(model);
	CREATE INDEX IF NOT EXISTS idx_token_usage_session ON token_usage(session_id);
	CREATE INDEX IF NOT EXISTS idx_token_usage_timestamp ON token_usage(timestamp);
	CREATE INDEX IF NOT EXISTS idx_sessions_start_time ON sessions(start_time);
	CREATE INDEX IF NOT EXISTS idx_sessions_indexed_at ON sessions(indexed_at);
	CREATE INDEX IF NOT EXISTS idx_token_usage_provider ON token_usage(provider);
	CREATE INDEX IF NOT EXISTS idx_sessions_working_directory ON sessions(working_directory);

	CREATE TABLE IF NOT EXISTS projects (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		path TEXT UNIQUE NOT NULL,
		name TEXT,
		last_activity DATETIME,
		status TEXT DEFAULT 'active',
		user_enabled INTEGER DEFAULT 1,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE INDEX IF NOT EXISTS idx_projects_status ON projects(status);
	CREATE INDEX IF NOT EXISTS idx_projects_last_activity ON projects(last_activity);

	CREATE TABLE IF NOT EXISTS schema_meta (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);
	`

	_, err = db.Exec(schema)
	if err != nil {
		return fmt.Errorf("failed to create schema: %w", err)
	}

	if err := runMigrations(); err != nil {
		return fmt.Errorf("failed to run migrations: %w", err)
	}

	opened = true
	return nil
}

// ForkName identifies this fork in schema_meta, so a database written by a
// different fork of mnemo is detectable rather than silently misread.
const ForkName = "terrillmoore/mnemo"

// upstreamMigrations is Pilan-AI/mnemo's migration list, kept verbatim and in
// order. Rebasing onto a newer upstream is then a straight copy of this slice.
// Never edit or reorder it, and never add to it: fork changes go in
// forkMigrations.
var upstreamMigrations = []string{
	"ALTER TABLE messages ADD COLUMN model TEXT DEFAULT ''",
	"ALTER TABLE messages ADD COLUMN provider TEXT DEFAULT ''",
	"ALTER TABLE messages ADD COLUMN input_tokens INTEGER DEFAULT 0",
	"ALTER TABLE messages ADD COLUMN output_tokens INTEGER DEFAULT 0",
	"ALTER TABLE messages ADD COLUMN cache_read_tokens INTEGER DEFAULT 0",
	"ALTER TABLE messages ADD COLUMN cache_write_tokens INTEGER DEFAULT 0",
	"ALTER TABLE messages ADD COLUMN cost_usd REAL DEFAULT 0.0",
	"ALTER TABLE messages ADD COLUMN message_uuid TEXT DEFAULT ''",
	"ALTER TABLE messages ADD COLUMN parent_uuid TEXT DEFAULT ''",
	"ALTER TABLE messages ADD COLUMN working_directory TEXT DEFAULT ''",
	"ALTER TABLE sessions ADD COLUMN provider TEXT DEFAULT ''",
	"ALTER TABLE sessions ADD COLUMN total_cache_read INTEGER DEFAULT 0",
	"ALTER TABLE sessions ADD COLUMN total_cache_write INTEGER DEFAULT 0",
	"ALTER TABLE sessions ADD COLUMN cli_version TEXT DEFAULT ''",
	"ALTER TABLE sessions ADD COLUMN git_branch TEXT DEFAULT ''",
	"ALTER TABLE sessions ADD COLUMN working_directory TEXT DEFAULT ''",
	"ALTER TABLE sessions ADD COLUMN start_time DATETIME",
	"ALTER TABLE sessions ADD COLUMN end_time DATETIME",
	"ALTER TABLE token_usage ADD COLUMN cache_read_tokens INTEGER DEFAULT 0",
	"ALTER TABLE token_usage ADD COLUMN cache_write_tokens INTEGER DEFAULT 0",
	"ALTER TABLE messages ADD COLUMN reasoning_tokens INTEGER DEFAULT 0",
	"ALTER TABLE messages ADD COLUMN agent TEXT DEFAULT ''",
	"ALTER TABLE messages ADD COLUMN date TEXT DEFAULT ''",
	"ALTER TABLE sessions ADD COLUMN total_reasoning_tokens INTEGER DEFAULT 0",
	"ALTER TABLE sessions ADD COLUMN agent TEXT DEFAULT ''",
	"ALTER TABLE sessions ADD COLUMN date TEXT DEFAULT ''",
}

// forkMigrations are this fork's own schema additions, applied after
// upstream's. Append only: the count of applied entries is recorded in
// schema_meta as fork_schema, and a database whose fork_schema exceeds
// len(forkMigrations) was written by a newer build and is refused.
// The index cannot live in the schema DDL above: on a database that predates
// the column, CREATE TABLE IF NOT EXISTS is a no-op and the index would be
// created against a sessions table that has no host yet.
var forkMigrations = []string{
	"ALTER TABLE sessions ADD COLUMN host TEXT NOT NULL DEFAULT ''",
	"CREATE INDEX IF NOT EXISTS idx_sessions_host ON sessions(host)",
}

// runMigrations applies schema additions idempotently.
//
// Upstream's statements run first, unversioned, exactly as upstream runs them:
// only "duplicate column name" errors are suppressed, and everything else
// (disk full, locked, corruption) propagates. The fork's statements then run
// under a version counter in schema_meta.
//
// Upstream stamps no version anywhere -- PRAGMA user_version is 0 on every
// database it has written -- so an old upstream binary can open a
// fork-upgraded file and write to it. Its inserts name their columns, so rows
// it writes simply take the default for anything it does not know about. That
// is why an empty host means "not yet claimed" rather than a valid value, and
// why `mnemo migrate host` exists to claim such rows later.
func runMigrations() error {
	for _, migration := range upstreamMigrations {
		_, err := db.Exec(migration)
		if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migration failed (%s): %w", migration, err)
		}
	}

	applied, err := forkSchemaVersion()
	if err != nil {
		return err
	}
	if applied > len(forkMigrations) {
		return fmt.Errorf(
			"database was written by a newer %s build (fork_schema %d, this build understands %d); upgrade mnemo",
			ForkName, applied, len(forkMigrations))
	}

	first := applied == 0
	for _, migration := range forkMigrations[applied:] {
		_, err := db.Exec(migration)
		if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("fork migration failed (%s): %w", migration, err)
		}
	}

	// The first time this file meets the fork, every existing session predates
	// the host column. They all came from this machine, so claim them. A
	// database the fork created is empty at this point and nothing is stamped.
	if first && len(forkMigrations) > 0 {
		n, err := ClaimEmptyHosts(Host())
		if err != nil {
			return fmt.Errorf("failed to claim existing sessions for host %q: %w", Host(), err)
		}
		if n > 0 {
			log.Printf("mnemo: claimed %d existing session(s) for host %q", n, Host())
		}
	}

	if err := setSchemaMeta("fork_name", ForkName); err != nil {
		return err
	}
	if err := setSchemaMetaInt("fork_schema", len(forkMigrations)); err != nil {
		return err
	}

	// Advisory only: upstream does not maintain this, we do. A stored value
	// above our own means some build knew more upstream migrations than we do,
	// so this fork needs rebasing onto a newer upstream.
	stored, err := schemaMetaInt("upstream_schema")
	if err != nil {
		return err
	}
	if stored > len(upstreamMigrations) {
		log.Printf("mnemo: database records %d upstream migrations but this build knows %d; %s needs rebasing onto a newer upstream",
			stored, len(upstreamMigrations), ForkName)
	} else if err := setSchemaMetaInt("upstream_schema", len(upstreamMigrations)); err != nil {
		return err
	}

	return nil
}

// schemaMeta reads one schema_meta value. A missing key returns "".
func schemaMeta(key string) (string, error) {
	var v string
	err := db.QueryRow("SELECT value FROM schema_meta WHERE key = ?", key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to read schema_meta %q: %w", key, err)
	}
	return v, nil
}

// schemaMetaInt reads one schema_meta value as an integer. A missing or
// unparseable value returns 0, so a database predating schema_meta reads as
// version 0 and every migration runs.
func schemaMetaInt(key string) (int, error) {
	v, err := schemaMeta(key)
	if err != nil || v == "" {
		return 0, err
	}
	n, convErr := strconv.Atoi(v)
	if convErr != nil {
		return 0, nil
	}
	return n, nil
}

func setSchemaMeta(key, value string) error {
	_, err := db.Exec(
		"INSERT INTO schema_meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		key, value)
	if err != nil {
		return fmt.Errorf("failed to write schema_meta %q: %w", key, err)
	}
	return nil
}

func setSchemaMetaInt(key string, value int) error {
	return setSchemaMeta(key, strconv.Itoa(value))
}

// forkSchemaVersion reports how many fork migrations this database has had
// applied.
func forkSchemaVersion() (int, error) {
	return schemaMetaInt("fork_schema")
}

// SchemaVersions reports the upstream and fork migration counts recorded in
// the database, and the fork that wrote it.
func SchemaVersions() (upstream, fork int, name string, err error) {
	if upstream, err = schemaMetaInt("upstream_schema"); err != nil {
		return
	}
	if fork, err = schemaMetaInt("fork_schema"); err != nil {
		return
	}
	name, err = schemaMeta("fork_name")
	return
}

// InitReadOnly opens the mnemo database for read-only access without running
// schema DDL. Use this for commands like inject that only need to search and
// must not block on write locks held by other processes.
func InitReadOnly() error {
	dbPath, err := Path()
	if err != nil {
		return err
	}
	if _, err := os.Stat(dbPath); err != nil {
		return fmt.Errorf("database not found: %w", err)
	}

	db, err = sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(2000)&_pragma=cache_size(-10000)&_pragma=temp_store(MEMORY)")
	if err != nil {
		db = nil
		return fmt.Errorf("failed to open database: %w", err)
	}

	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		db = nil
		return fmt.Errorf("failed to connect to database: %w", err)
	}

	return nil
}

// CloseDB closes the global database connection.
func CloseDB() {
	if db != nil {
		if err := db.Close(); err != nil {
			log.Printf("warning: failed to close database cleanly: %v", err)
		}
		db = nil
	}
}

// execer abstracts *sql.DB and *sql.Tx for shared insert/delete helpers.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// BeginTx starts a new database transaction for atomic multi-step operations.
func BeginTx() (*sql.Tx, error) {
	return db.Begin()
}

// OpenReadOnlySQLite opens an external SQLite database in read-only mode
// for indexing tool-specific databases (e.g. Cursor's state.vscdb, Crush's crush.db).
func OpenReadOnlySQLite(path string) (*sql.DB, error) {
	conn, err := sql.Open("sqlite", path+"?mode=ro&_busy_timeout=3000")
	if err != nil {
		return nil, err
	}
	if err := conn.Ping(); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("failed to open %s: %w", filepath.Base(path), err)
	}
	return conn, nil
}

// ClearIndex drops all data from messages, sessions, and token_usage tables.
func ClearIndex() error {
	// messages_fts cleanup is handled by the messages_ad trigger on DELETE,
	// so we only need to delete from the base tables.
	_, err := db.Exec(`
		DELETE FROM messages;
		DELETE FROM sessions;
		DELETE FROM token_usage;
	`)
	return err
}
