package auth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"livelyboard/internal/db"
)

var secret = []byte("test-secret-that-is-at-least-32-bytes!")

func newServer(t *testing.T) (*httptest.Server, *Store) {
	t.Helper()
	conn, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	s, err := New(conn, secret)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	Routes(mux, s)
	// a protected route, to exercise Require end to end
	mux.HandleFunc("GET /me", Require(secret, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(UserID(r.Context())))
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, s
}

func post(t *testing.T, url, body string, out any) int {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func me(t *testing.T, url, tok string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url+"/me", nil)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(b))
}

const creds = `{"username":"alice","password":"correct-horse"}`

func TestRegisterLoginRefresh(t *testing.T) {
	srv, _ := newServer(t)

	var u User
	if code := post(t, srv.URL+"/auth/register", creds, &u); code != 201 || u.ID == "" || u.Username != "alice" {
		t.Fatalf("register: %d %+v", code, u)
	}

	var tok Tokens
	if code := post(t, srv.URL+"/auth/login", creds, &tok); code != 200 || tok.AccessToken == "" || tok.RefreshToken == "" {
		t.Fatalf("login: %d %+v", code, tok)
	}
	if code, sub := me(t, srv.URL, tok.AccessToken); code != 200 || sub != u.ID {
		t.Fatalf("access token: %d %q, want 200 %q", code, sub, u.ID)
	}

	var next Tokens
	if code := post(t, srv.URL+"/auth/refresh", `{"refresh_token":"`+tok.RefreshToken+`"}`, &next); code != 200 || next.RefreshToken == tok.RefreshToken {
		t.Fatalf("refresh: %d %+v", code, next)
	}
	if code, sub := me(t, srv.URL, next.AccessToken); code != 200 || sub != u.ID {
		t.Fatalf("refreshed access token: %d %q", code, sub)
	}
	// rotation: the old refresh token is gone
	if code := post(t, srv.URL+"/auth/refresh", `{"refresh_token":"`+tok.RefreshToken+`"}`, nil); code != 401 {
		t.Fatalf("reused refresh token: got %d, want 401", code)
	}
}

func TestAuthErrors(t *testing.T) {
	srv, s := newServer(t)
	post(t, srv.URL+"/auth/register", creds, nil)

	expired, err := s.issue(context.Background(), s.db.Write, "alice-id", time.Now().Add(-RefreshTTL-time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, path, body string
		want             int
	}{
		{"duplicate username", "/auth/register", creds, 409},
		{"short password", "/auth/register", `{"username":"bob","password":"short"}`, 400},
		{"short username", "/auth/register", `{"username":"b","password":"long-enough"}`, 400},
		{"password over 72 bytes", "/auth/register", `{"username":"bob","password":"` + strings.Repeat("x", 73) + `"}`, 400},
		{"bad json register", "/auth/register", `{`, 400},
		{"wrong password", "/auth/login", `{"username":"alice","password":"wrong-password"}`, 401},
		{"unknown user", "/auth/login", `{"username":"nobody","password":"whatever-pass"}`, 401},
		{"bad json login", "/auth/login", `{`, 400},
		{"unknown refresh token", "/auth/refresh", `{"refresh_token":"nope"}`, 401},
		{"expired refresh token", "/auth/refresh", `{"refresh_token":"` + expired.RefreshToken + `"}`, 401},
		{"bad json refresh", "/auth/refresh", `{`, 400},
	}
	for _, c := range cases {
		if code := post(t, srv.URL+c.path, c.body, nil); code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, code, c.want)
		}
	}
}

func TestRequire(t *testing.T) {
	srv, _ := newServer(t)
	now := time.Now()
	wrongKey, _ := NewAccessToken([]byte("another-secret-that-is-32-bytes-long"), "alice", now)
	expired, _ := NewAccessToken(secret, "alice", now.Add(-time.Hour))
	noSub, _ := NewAccessToken(secret, "", now)

	for name, tok := range map[string]string{"none": "", "garbage": "x.y.z", "wrong key": wrongKey, "expired": expired, "empty sub": noSub} {
		if code, _ := me(t, srv.URL, tok); code != 401 {
			t.Errorf("%s: got %d, want 401", name, code)
		}
	}
}

func TestLogout(t *testing.T) {
	srv, _ := newServer(t)
	post(t, srv.URL+"/auth/register", creds, nil)
	var tok Tokens
	post(t, srv.URL+"/auth/login", creds, &tok)
	body := `{"refresh_token":"` + tok.RefreshToken + `"}`

	if code := post(t, srv.URL+"/auth/logout", body, nil); code != 204 {
		t.Fatalf("logout: got %d, want 204", code)
	}
	if code := post(t, srv.URL+"/auth/refresh", body, nil); code != 401 {
		t.Fatalf("refresh after logout: got %d, want 401", code)
	}
	if code := post(t, srv.URL+"/auth/logout", body, nil); code != 204 {
		t.Fatalf("second logout: got %d, want 204", code)
	}
	if code := post(t, srv.URL+"/auth/logout", `{`, nil); code != 400 {
		t.Fatalf("bad json logout: got %d, want 400", code)
	}
}

func TestDeleteExpiredRefreshTokens(t *testing.T) {
	_, s := newServer(t)
	ctx := context.Background()
	now := time.Now()
	if _, err := s.issue(ctx, s.db.Write, "u", now.Add(-RefreshTTL-time.Hour)); err != nil {
		t.Fatal(err)
	}
	live, err := s.issue(ctx, s.db.Write, "u", now)
	if err != nil {
		t.Fatal(err)
	}

	if n, err := s.DeleteExpiredRefreshTokens(ctx, now); err != nil || n != 1 {
		t.Fatalf("deleted %d (err %v), want 1", n, err)
	}
	var left string
	if err := s.db.Read.QueryRow(`SELECT token_hash FROM refresh_tokens`).Scan(&left); err != nil || left != hashToken(live.RefreshToken) {
		t.Fatalf("remaining token %q (err %v), want the live one", left, err)
	}
}
