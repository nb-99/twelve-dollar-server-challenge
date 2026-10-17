package api_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"

	"github.com/nb-99/twelve-dollar-server-challenge/submissions/go-fiber-nb-99/internal/api"
	"github.com/nb-99/twelve-dollar-server-challenge/submissions/go-fiber-nb-99/internal/auth"
	"github.com/nb-99/twelve-dollar-server-challenge/submissions/go-fiber-nb-99/internal/store"
	"github.com/nb-99/twelve-dollar-server-challenge/submissions/go-fiber-nb-99/internal/testdb"
)

const secret = "test-secret"

// testConfig allows slow builds, such as under emulation, more than the default second.
var testConfig = fiber.TestConfig{Timeout: 10 * time.Second, FailOnTimeout: true}

// newApp serves a testdb database.
func newApp(t *testing.T) *fiber.App {
	t.Helper()
	s, err := store.Open(testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return api.New(s, auth.New([]byte(secret)))
}

// do sends a request and returns its status and body. An empty authorization
// sends no Authorization header. Every response must be JSON.
func do(t *testing.T, app *fiber.App, method, target, authorization, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := app.Test(req, testConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if contentType := resp.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Errorf("%s %s: Content-Type %q", method, target, contentType)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(data)
}

func expect(t *testing.T, app *fiber.App, method, target, authorization, body string, status int, want string) {
	t.Helper()
	gotStatus, got := do(t, app, method, target, authorization, body)
	if gotStatus != status || got != want {
		t.Errorf("%s %s %s: got %d %s, want %d %s", method, target, body, gotStatus, got, status, want)
	}
}

// token signs an HS256 token for user 1 with username "token-name". Each
// override sets a claim, or removes it when the value is nil.
func token(t *testing.T, overrides map[string]any) string {
	t.Helper()
	now := time.Now().Unix()
	claims := jwt.MapClaims{"sub": "1", "username": "token-name", "iat": now, "exp": now + 3600}
	for key, value := range overrides {
		if value == nil {
			delete(claims, key)
		} else {
			claims[key] = value
		}
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	return "Bearer " + signed
}

func TestHealthAndUnknownPath(t *testing.T) {
	app := newApp(t)
	status, body := do(t, app, "GET", "/health", "", "")
	if status != 200 || !regexp.MustCompile(`^\{"status":"ok","db":"ok","uptime_s":[0-9]+\}$`).MatchString(body) {
		t.Errorf("GET /health: got %d %s", status, body)
	}
	expect(t, app, "GET", "/nope", "", "", 404, `{"error":"not found"}`)
	expect(t, app, "POST", "/feed/x", "", "", 404, `{"error":"not found"}`)
}

func TestHealthReportsUnreachableDatabase(t *testing.T) {
	s, err := store.Open(testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	status, body := do(t, api.New(s, auth.New([]byte(secret))), "GET", "/health", "", "")
	if status != 503 || !regexp.MustCompile(`^\{"status":"degraded","db":"unreachable","error":"[^"]+"\}$`).MatchString(body) {
		t.Errorf("GET /health: got %d %s", status, body)
	}
}

func TestReadsPostsAndFeed(t *testing.T) {
	app := newApp(t)
	post1 := `{"id":1,"body":"hello \"world\" é","created_at":"2025-01-01T00:01:00.000Z","author":"alice","like_count":2}`
	expect(t, app, "GET", "/posts/1", "", "", 200, `{"post":`+post1+`}`)
	status, body := do(t, app, "GET", "/feed", "", "")
	want := `{"posts":[` + post1 + `,{"id":22,"body":"post 22","created_at":"2025-01-01T00:00:21.000Z","author":"bob","like_count":0},`
	if status != 200 || !strings.HasPrefix(body, want) || strings.Count(body, `"id":`) != 20 {
		t.Errorf("GET /feed: got %d %s, want 20 posts starting %s", status, body, want)
	}
}

func TestValidatesPostIDs(t *testing.T) {
	app := newApp(t)
	valid := token(t, nil)
	for _, id := range []string{"0", "-1", "abc", "1.5", "+1", "%201", "99999999999999999999"} {
		expect(t, app, "GET", "/posts/"+id, "", "", 400, `{"error":"invalid post id"}`)
		expect(t, app, "POST", "/posts/"+id+"/like", valid, "", 400, `{"error":"invalid post id"}`)
	}
	expect(t, app, "GET", "/posts/999", "", "", 404, `{"error":"post not found"}`)
	expect(t, app, "POST", "/posts/999/like", valid, "", 404, `{"error":"post not found"}`)
}

func TestAuthenticationPrecedesValidation(t *testing.T) {
	app := newApp(t)
	cases := []struct{ authorization, message string }{
		{"", "missing bearer token"},
		{"Bearer abc", "invalid or expired token"},
		{token(t, map[string]any{"sub": "abc"}), "invalid token payload"},
	}
	for _, c := range cases {
		want := `{"error":"` + c.message + `"}`
		expect(t, app, "POST", "/posts", c.authorization, "{bad json", 401, want)
		expect(t, app, "POST", "/posts/abc/like", c.authorization, "", 401, want)
	}
}

func TestCreatePostValidation(t *testing.T) {
	app := newApp(t)
	valid := token(t, nil)
	cases := []struct{ body, message string }{
		{"", "malformed JSON body"},
		{"{bad json", "malformed JSON body"},
		{`{"body":"x"} trailing`, "malformed JSON body"},
		{`{}`, "body is required"},
		{`[]`, "body is required"},
		{`"text"`, "body is required"},
		{`{"body":null}`, "body is required"},
		{`{"body":123}`, "body is required"},
		{`{"body":""}`, "body is required"},
		{`{"body":" \t\n "}`, "body is required"},
		{`{"Body":"x"}`, "body is required"},
		{`{"body":"` + strings.Repeat("a", 501) + `"}`, "body must be at most 500 characters"},
		{`{"body":"` + strings.Repeat("é", 501) + `"}`, "body must be at most 500 characters"},
	}
	for _, c := range cases {
		expect(t, app, "POST", "/posts", valid, c.body, 400, `{"error":"`+c.message+`"}`)
	}
}

func TestCreateAndLikePost(t *testing.T) {
	app := newApp(t)
	valid := token(t, nil)
	text := strings.Repeat("é", 500)

	status, body := do(t, app, "POST", "/posts", valid, `{"body":"  `+text+` \n"}`)
	var created struct {
		Post struct {
			CreatedAt string `json:"created_at"`
		} `json:"post"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("POST /posts: %d %s", status, body)
	}
	if !regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`).MatchString(created.Post.CreatedAt) {
		t.Errorf("POST /posts: created_at %q", created.Post.CreatedAt)
	}
	// The new post's author is the token's username; reads use users.username.
	post := func(author string, likes int) string {
		return fmt.Sprintf(`{"id":23,"body":"%s","created_at":"%s","author":"%s","like_count":%d}`,
			text, created.Post.CreatedAt, author, likes)
	}
	if want := `{"post":` + post("token-name", 0) + `}`; status != 201 || body != want {
		t.Errorf("POST /posts: got %d %s, want 201 %s", status, body, want)
	}

	expect(t, app, "POST", "/posts/23/like", valid, "", 201, `{"liked":true,"already_liked":false,"post_id":23}`)
	expect(t, app, "POST", "/posts/23/like", valid, "", 200, `{"liked":true,"already_liked":true,"post_id":23}`)
	expect(t, app, "GET", "/posts/23", "", "", 200, `{"post":`+post("alice", 1)+`}`)
	status, body = do(t, app, "GET", "/feed", "", "")
	if want := `{"posts":[` + post("alice", 1) + `,`; status != 200 || !strings.HasPrefix(body, want) {
		t.Errorf("GET /feed: got %d %s, want prefix %s", status, body, want)
	}
}

func TestSpecialCharactersRoundTrip(t *testing.T) {
	app := newApp(t)
	// Escaping <>&/ and non-ASCII characters is optional; control characters are escaped.
	status, created := do(t, app, "POST", "/posts", token(t, nil), `{"body":" <b>&amp;</b> \"q\" / café ✓\nx\u0001 "}`)
	if status != 201 || !strings.Contains(created, `"body":"<b>&amp;</b> \"q\" / café ✓\nx\u0001"`) {
		t.Errorf("POST /posts: got %d %s", status, created)
	}
	// Reads name the author from users.username, the create response from the token.
	expect(t, app, "GET", "/posts/23", "", "", 200, strings.Replace(created, `"author":"token-name"`, `"author":"alice"`, 1))
}

func TestConcurrentLikesCountOnce(t *testing.T) {
	app := newApp(t)
	valid := token(t, nil)
	statuses := make(chan int, 20)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			req := httptest.NewRequest("POST", "/posts/5/like", nil)
			req.Header.Set("Authorization", valid)
			resp, err := app.Test(req, testConfig)
			if err != nil {
				statuses <- 0
				return
			}
			resp.Body.Close()
			statuses <- resp.StatusCode
		})
	}
	wg.Wait()
	close(statuses)
	counts := map[int]int{}
	for status := range statuses {
		counts[status]++
	}
	if counts[201] != 1 || counts[200] != 19 {
		t.Errorf("statuses: got %v, want one 201 and nineteen 200", counts)
	}
	expect(t, app, "GET", "/posts/5", "", "", 200,
		`{"post":{"id":5,"body":"post 5","created_at":"2025-01-01T00:00:05.000Z","author":"bob","like_count":1}}`)
}

func TestUnexpectedErrorsAreInternal(t *testing.T) {
	app := newApp(t)
	// The foreign key rejects a post by a user who is not in the database.
	expect(t, app, "POST", "/posts", token(t, map[string]any{"sub": "999"}), `{"body":"x"}`,
		500, `{"error":"internal server error"}`)
}
