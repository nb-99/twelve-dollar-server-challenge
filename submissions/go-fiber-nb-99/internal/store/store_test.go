package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nb-99/twelve-dollar-server-challenge/submissions/go-fiber-nb-99/internal/store"
	"github.com/nb-99/twelve-dollar-server-challenge/submissions/go-fiber-nb-99/internal/testdb"
)

const post1 = `{"id":1,"body":"hello \"world\" é","created_at":"2025-01-01T00:01:00.000Z","author":"alice","like_count":2}`

func open(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := testdb.New(t)
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

// exec runs statements on a separate connection to the database at path.
func exec(t *testing.T, path string, statements ...string) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOpenRequiresExistingDatabase(t *testing.T) {
	if _, err := store.Open(filepath.Join(t.TempDir(), "missing.db")); err == nil {
		t.Error("Open created a missing database")
	}
}

func TestOpenSwitchesToWAL(t *testing.T) {
	path := testdb.New(t)
	exec(t, path, "PRAGMA journal_mode = DELETE")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Errorf("journal_mode = %q, %v; want wal", mode, err)
	}
}

func TestFeedOrdersByTimestampThenID(t *testing.T) {
	s, _ := open(t)
	body, err := s.FeedJSON(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(body), `{"posts":[`+post1+`,`) {
		t.Errorf("feed starts %.200s, want post 1", body)
	}
	var feed struct{ Posts []struct{ ID int64 } }
	if err := json.Unmarshal(body, &feed); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, post := range feed.Posts {
		ids = append(ids, post.ID)
	}
	want := []int64{1, 22, 21, 20, 19, 18, 17, 16, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4}
	if !slices.Equal(ids, want) {
		t.Errorf("feed ids = %v, want %v", ids, want)
	}
}

func TestFeedWithoutPosts(t *testing.T) {
	s, path := open(t)
	exec(t, path, "DELETE FROM likes", "DELETE FROM posts")
	body, err := s.FeedJSON(context.Background())
	if err != nil || string(body) != `{"posts":[]}` {
		t.Errorf("FeedJSON = %s, %v; want {\"posts\":[]}", body, err)
	}
}

func TestPostJSON(t *testing.T) {
	s, _ := open(t)
	body, err := s.PostJSON(context.Background(), 1)
	if want := `{"post":` + post1 + `}`; err != nil || string(body) != want {
		t.Errorf("PostJSON(1) = %s, %v; want %s", body, err, want)
	}
	if _, err := s.PostJSON(context.Background(), 999); !errors.Is(err, store.ErrPostNotFound) {
		t.Errorf("PostJSON(999) error = %v, want ErrPostNotFound", err)
	}
}

func TestCreatePostIsCommitted(t *testing.T) {
	s, path := open(t)
	body, err := s.CreatePostJSON(context.Background(), 1, "token-name", "new <b>&</b>\nline")
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		Post struct {
			CreatedAt string `json:"created_at"`
		} `json:"post"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("CreatePostJSON = %s: %v", body, err)
	}
	want := `{"post":{"id":23,"body":"new <b>&</b>\nline","created_at":"` + created.Post.CreatedAt +
		`","author":"token-name","like_count":0}}`
	if string(body) != want {
		t.Errorf("CreatePostJSON = %s, want %s", body, want)
	}

	// Another connection sees the row, so it was committed before CreatePostJSON returned.
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var userID int64
	var text, createdAt string
	if err := db.QueryRow("SELECT user_id, body, created_at FROM posts WHERE id = 23").Scan(&userID, &text, &createdAt); err != nil {
		t.Fatal(err)
	}
	if userID != 1 || text != "new <b>&</b>\nline" || createdAt != created.Post.CreatedAt {
		t.Errorf("stored user %d, body %q, created_at %q", userID, text, createdAt)
	}
}

func TestCreatePostRejectsUnknownUser(t *testing.T) {
	s, _ := open(t)
	if _, err := s.CreatePostJSON(context.Background(), 999, "ghost", "x"); err == nil {
		t.Error("CreatePostJSON accepted a user who is not in the database")
	}
}

func TestLike(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	for i, want := range []bool{false, true} {
		alreadyLiked, err := s.Like(ctx, 1, 5)
		if err != nil || alreadyLiked != want {
			t.Errorf("Like #%d = %v, %v; want %v", i+1, alreadyLiked, err, want)
		}
	}
	body, err := s.PostJSON(ctx, 5)
	if err != nil || !strings.HasSuffix(string(body), `"like_count":1}}`) {
		t.Errorf("PostJSON(5) = %s, %v; want like_count 1", body, err)
	}
	if _, err := s.Like(ctx, 1, 999); !errors.Is(err, store.ErrPostNotFound) {
		t.Errorf("Like(999) error = %v, want ErrPostNotFound", err)
	}
}
