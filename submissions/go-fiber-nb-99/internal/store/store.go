// Package store reads and writes posts and likes in the challenge's SQLite
// database. Reads return the API's JSON bodies, which SQLite's JSON functions
// build, so the posts' rows never cross into Go one column at a time.
package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"

	"github.com/mattn/go-sqlite3"
)

// readers bounds the read connections, so thousands of concurrent requests
// queue for a connection instead of each opening its own. Writes use a single
// connection: SQLite runs one write transaction at a time anyway, and queueing
// in Go avoids waiting in SQLite's busy handler.
const readers = 4

// ErrPostNotFound reports a post id without a row in posts.
var ErrPostNotFound = errors.New("post not found")

// mmapSize maps the database file, about 240 MB, into memory instead of
// copying its pages into each connection's page cache.
const mmapSize = 1 << 30

var (
	// writerPragmas make commits durable (synchronous=NORMAL in WAL mode is the
	// weakest setting the challenge allows) and enforce the schema's foreign
	// keys. setUpWriter also switches to WAL mode.
	writerPragmas = []string{
		"busy_timeout = 5000",
		"synchronous = NORMAL",
		"foreign_keys = ON",
		fmt.Sprintf("mmap_size = %d", mmapSize),
	}
	readerPragmas = []string{
		"busy_timeout = 5000",
		fmt.Sprintf("mmap_size = %d", mmapSize),
		"query_only = ON",
	}
)

// selectPosts is the reference query from SPEC.md.
const selectPosts = `
SELECT p.id, p.body, p.created_at, u.username AS author,
       (SELECT count(*) FROM likes l WHERE l.post_id = p.id) AS like_count
  FROM posts p JOIN users u ON u.id = p.user_id`

// postObject is a post as JSON, with its keys in the order of SPEC.md.
const postObject = `json_object('id', id, 'body', body, 'created_at', created_at, 'author', author, 'like_count', like_count)`

const (
	feedQuery = `SELECT json_object('posts', json_group_array(` + postObject + ` ORDER BY created_at DESC, id DESC))
  FROM (` + selectPosts + ` ORDER BY p.created_at DESC, p.id DESC LIMIT 20)`
	postQuery       = `SELECT json_object('post', ` + postObject + `) FROM (` + selectPosts + ` WHERE p.id = ?)`
	postExistsQuery = `SELECT EXISTS (SELECT 1 FROM posts WHERE id = ?)`
	// The author is the token's username, which is not looked up.
	insertPost = `INSERT INTO posts (user_id, body) VALUES (?1, ?2)
RETURNING json_object('post', json_object('id', id, 'body', body, 'created_at', created_at, 'author', ?3, 'like_count', 0))`
	// The SELECT inserts nothing for a missing post. Its WHERE clause also
	// keeps ON CONFLICT from parsing as a join constraint.
	insertLike = `INSERT INTO likes (user_id, post_id) SELECT ?, id FROM posts WHERE id = ?
ON CONFLICT (user_id, post_id) DO NOTHING`
)

type Store struct {
	reader, writer         *sql.DB
	feed, post, postExists *sql.Stmt
	insertPost, insertLike *sql.Stmt
}

// Open opens the existing database at path; it never creates one.
func Open(path string) (*Store, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// _mutex=no drops SQLite's per-connection mutex: database/sql never uses
	// a connection from two goroutines at once.
	dsn := (&url.URL{
		Scheme:   "file",
		Path:     filepath.ToSlash(absolute),
		RawQuery: "mode=rw&_mutex=no",
	}).String()

	s := &Store{
		writer: openPool(dsn, 1, setUpWriter),
		reader: openPool(dsn, readers, setUpReader),
	}
	fail := func(err error) (*Store, error) {
		s.Close()
		return nil, err
	}
	// The writer connects first, so the database is in WAL mode before any reader.
	for _, db := range []*sql.DB{s.writer, s.reader} {
		if err := db.Ping(); err != nil {
			return fail(fmt.Errorf("open %s: %w", path, err))
		}
	}
	statements := []struct {
		db    *sql.DB
		query string
		stmt  **sql.Stmt
	}{
		{s.reader, feedQuery, &s.feed},
		{s.reader, postQuery, &s.post},
		{s.reader, postExistsQuery, &s.postExists},
		{s.writer, insertPost, &s.insertPost},
		{s.writer, insertLike, &s.insertLike},
	}
	for _, st := range statements {
		if *st.stmt, err = st.db.Prepare(st.query); err != nil {
			return fail(fmt.Errorf("prepare %q: %w", st.query, err))
		}
	}
	return s, nil
}

func openPool(dsn string, size int, setUp func(*sqlite3.SQLiteConn) error) *sql.DB {
	db := sql.OpenDB(connector{dsn: dsn, driver: &sqlite3.SQLiteDriver{ConnectHook: setUp}})
	db.SetMaxOpenConns(size)
	db.SetMaxIdleConns(size)
	return db
}

func setUpWriter(conn *sqlite3.SQLiteConn) error {
	if err := setPragmas(conn, writerPragmas); err != nil {
		return err
	}
	// The pragma reports the mode in effect instead of failing when it cannot switch.
	rows, err := conn.Query("PRAGMA journal_mode = WAL", nil)
	if err != nil {
		return err
	}
	defer rows.Close()
	mode := make([]driver.Value, 1)
	if err := rows.Next(mode); err != nil {
		return fmt.Errorf("PRAGMA journal_mode: %w", err)
	}
	if fmt.Sprintf("%s", mode[0]) != "wal" {
		return fmt.Errorf("journal_mode is %s, not wal", mode[0])
	}
	return nil
}

func setUpReader(conn *sqlite3.SQLiteConn) error {
	return setPragmas(conn, readerPragmas)
}

func setPragmas(conn *sqlite3.SQLiteConn, pragmas []string) error {
	for _, pragma := range pragmas {
		if _, err := conn.Exec("PRAGMA "+pragma, nil); err != nil {
			return fmt.Errorf("PRAGMA %s: %w", pragma, err)
		}
	}
	return nil
}

// connector opens connections with a driver whose ConnectHook sets them up.
type connector struct {
	dsn    string
	driver *sqlite3.SQLiteDriver
}

func (c connector) Connect(context.Context) (driver.Conn, error) { return c.driver.Open(c.dsn) }
func (c connector) Driver() driver.Driver                        { return c.driver }

// Close closes both pools and, with their connections, the prepared statements.
func (s *Store) Close() error {
	return errors.Join(s.reader.Close(), s.writer.Close())
}

// Ping runs SELECT 1 on a read connection.
func (s *Store) Ping(ctx context.Context) error {
	var one int
	return s.reader.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}

// FeedJSON returns {"posts":[...]} with the 20 newest posts.
func (s *Store) FeedJSON(ctx context.Context) ([]byte, error) {
	var body []byte
	err := s.feed.QueryRowContext(ctx).Scan(&body)
	return body, err
}

// PostJSON returns {"post":{...}} for post id.
func (s *Store) PostJSON(ctx context.Context, id int64) ([]byte, error) {
	var body []byte
	err := s.post.QueryRowContext(ctx, id).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPostNotFound
	}
	return body, err
}

// CreatePostJSON inserts a post by userID and, once the insert is committed,
// returns {"post":{...}} with author as its author.
func (s *Store) CreatePostJSON(ctx context.Context, userID int64, author, body string) ([]byte, error) {
	var response []byte
	// Scan closes the statement's rows, which ends its auto-commit
	// transaction and returns a failed commit as an error.
	err := s.insertPost.QueryRowContext(ctx, userID, body, author).Scan(&response)
	return response, err
}

// Like records one like per user and post and reports whether it already existed.
func (s *Store) Like(ctx context.Context, userID, postID int64) (alreadyLiked bool, err error) {
	result, err := s.insertLike.ExecContext(ctx, userID, postID)
	if err != nil {
		return false, err
	}
	inserted, err := result.RowsAffected()
	if err != nil || inserted == 1 {
		return false, err
	}
	// Nothing was inserted: the like exists, or the post does not.
	var exists bool
	if err := s.postExists.QueryRowContext(ctx, postID).Scan(&exists); err != nil {
		return false, err
	}
	if !exists {
		return false, ErrPostNotFound
	}
	return true, nil
}
