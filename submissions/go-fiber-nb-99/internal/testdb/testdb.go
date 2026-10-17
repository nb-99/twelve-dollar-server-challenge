// Package testdb creates small SQLite databases with the challenge schema for tests.
package testdb

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// New creates a database in a temporary directory and returns its path. It
// holds users 1 (alice) and 2 (bob) and 22 posts:
//   - post 1, by alice, 'hello "world" é' at 2025-01-01T00:01:00.000Z, the
//     newest, liked by both users;
//   - posts 2–21, by bob, 'post N' at 2025-01-01T00:00:NN.000Z;
//   - post 22, by bob, 'post 22' with the same timestamp as post 21.
//
// The file name contains URI-special characters, so opening it checks that
// callers escape the path.
func New(t testing.TB) string {
	t.Helper()
	schema, err := os.ReadFile(schemaPath())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "feed #1 %41.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	statements := []string{
		string(schema),
		"PRAGMA journal_mode = WAL",
		"INSERT INTO users (id, username) VALUES (1, 'alice'), (2, 'bob')",
		`INSERT INTO posts (id, user_id, body, created_at)
		 VALUES (1, 1, 'hello "world" é', '2025-01-01T00:01:00.000Z')`,
		"INSERT INTO likes (user_id, post_id) VALUES (1, 1), (2, 1)",
	}
	for id := 2; id <= 22; id++ {
		statements = append(statements, fmt.Sprintf(
			"INSERT INTO posts (id, user_id, body, created_at) VALUES (%d, 2, 'post %d', '2025-01-01T00:00:%02d.000Z')",
			id, id, min(id, 21)))
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// schemaPath is the repository's schema.sql, four directories above this file.
func schemaPath() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "schema.sql")
}
