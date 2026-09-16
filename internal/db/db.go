package db

import (
	"crypto/rand"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	_ "modernc.org/sqlite"
)

var (
	entropyMu sync.Mutex
	entropy   = ulid.Monotonic(rand.Reader, 0)
)

// DB wraps a SQLite database connection.
type DB struct {
	sql *sql.DB
}

// Open opens (or creates) the SQLite database at path and runs migrations.
func Open(path string) (*DB, error) {
	sqlDB, err := sql.Open("sqlite", path+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if _, err := sqlDB.Exec(schemaSQL); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate db: %w", err)
	}
	for _, stmt := range migrationStatements {
		if _, err := sqlDB.Exec(stmt); err != nil && !isDuplicateColumnErr(err) {
			sqlDB.Close()
			return nil, fmt.Errorf("migrate db: %w", err)
		}
	}
	if err := assertReviewQuestionsShape(sqlDB); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return &DB{sql: sqlDB}, nil
}

// assertReviewQuestionsShape refuses a review_questions table that predates
// ask_ordinal instead of running against it.
//
// There is deliberately no migration for this. The table is new, so no released
// version ships the old shape - CREATE TABLE gives every upstream database the
// five-column key - and migrationStatements cannot carry the remedy anyway:
// they are re-executed with their errors TOLERATED on every start, which is
// safe for an additive ALTER and destructive for the create/copy/drop/rename a
// key change needs. SQLite cannot ALTER a column into a primary key.
//
// Only a database that ran an earlier commit of this feature's own branch can
// have the old shape, and there the failure was silent and total: every INSERT
// and every SELECT names ask_ordinal, so a human's settled decision reached
// neither the do-not-re-raise section nor the PR body while nothing errored.
// Refusing to open says so once, and the operator drops the table by hand -
// nothing here modifies any live database.
func assertReviewQuestionsShape(sqlDB *sql.DB) error {
	rows, err := sqlDB.Query(`SELECT name FROM pragma_table_info('review_questions')`)
	if err != nil {
		return fmt.Errorf("inspect review_questions: %w", err)
	}
	defer rows.Close()
	exists := false
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			return fmt.Errorf("inspect review_questions: %w", err)
		}
		exists = true
		if column == "ask_ordinal" {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inspect review_questions: %w", err)
	}
	if !exists {
		return nil
	}
	return fmt.Errorf("review_questions predates its ask_ordinal column, so every settled review answer would be silently lost; drop the table (sqlite3 <db> 'DROP TABLE review_questions') and restart to have it recreated")
}

// OpenReadOnly opens an existing database without creating or migrating it.
// It is used by pre-mutation authorization, where even schema repair would be
// an unacceptable side effect before the caller is classified.
func OpenReadOnly(path string) (*DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	sqlDB, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open db read-only: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("open db read-only: %w", err)
	}
	return &DB{sql: sqlDB}, nil
}

// isDuplicateColumnErr reports whether err is SQLite's "duplicate column name"
// error, which ALTER TABLE ADD COLUMN emits when the column already exists.
// Treating this as a no-op keeps migrations idempotent without a version table.
func isDuplicateColumnErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "duplicate column name")
}

// Close closes the database connection.
func (d *DB) Close() error {
	return d.sql.Close()
}

// newID generates a new ULID with monotonic ordering.
func newID() string {
	entropyMu.Lock()
	defer entropyMu.Unlock()
	return ulid.MustNew(ulid.Timestamp(time.Now()), entropy).String()
}

// now returns the current unix timestamp in seconds.
func now() int64 {
	return time.Now().Unix()
}
