package team

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"livelyboard/internal/auth"
	"livelyboard/internal/db"
)

var secret = []byte("test-secret-that-is-at-least-32-bytes!")

func newStore(t *testing.T) *Store {
	t.Helper()
	conn, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	s, err := New(conn)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	Routes(mux, newStore(t), secret)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func token(t *testing.T, key []byte, userID string, now time.Time) string {
	t.Helper()
	tok, err := auth.NewAccessToken(key, userID, now)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func post(t *testing.T, url, tok, body string) (int, Team) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var team Team
	json.NewDecoder(resp.Body).Decode(&team) // error bodies are plain text; team stays zero
	return resp.StatusCode, team
}

// eventAt returns a valid UUIDv7 carrying timestamp ts.
func eventAt(ts time.Time) string {
	u := uuid.Must(uuid.NewV7())
	at := uuidAt(ts)
	copy(u[:6], at[:6])
	return u.String()
}

func TestCreateAndAddScore(t *testing.T) {
	srv := newServer(t)
	alice := token(t, secret, "alice", time.Now())

	code, team := post(t, srv.URL+"/teams", alice, `{"name":"red","score":10}`)
	if code != http.StatusCreated || team.Name != "red" || team.Score != 10 || team.ID == "" || team.OwnerID != "alice" {
		t.Fatalf("create: got %d %+v", code, team)
	}
	scoreURL := srv.URL + "/teams/" + team.ID + "/score"
	e1, e2 := eventAt(time.Now()), eventAt(time.Now())

	if code, got := post(t, scoreURL, alice, `{"event_id":"`+e1+`","delta":5}`); code != 200 || got.Score != 15 {
		t.Fatalf("add: got %d %+v, want 200 score 15", code, got)
	}
	// a retry of the same event must not be counted twice, even if sent in uppercase
	if code, got := post(t, scoreURL, alice, `{"event_id":"`+strings.ToUpper(e1)+`","delta":5}`); code != 200 || got.Score != 15 {
		t.Fatalf("retry: got %d %+v, want 200 score 15", code, got)
	}
	if code, got := post(t, scoreURL, alice, `{"event_id":"`+e2+`","delta":-3}`); code != 200 || got.Score != 12 {
		t.Fatalf("second event: got %d %+v, want 200 score 12", code, got)
	}
}

func TestErrors(t *testing.T) {
	srv := newServer(t)
	now := time.Now()
	alice := token(t, secret, "alice", now)
	_, team := post(t, srv.URL+"/teams", alice, `{"name":"red","score":0}`)
	score := "/teams/" + team.ID + "/score"
	ev := func(id string) string { return `{"event_id":"` + id + `","delta":1}` }

	cases := []struct {
		name, path, tok, body string
		want                  int
	}{
		{"create without token", "/teams", "", `{"name":"x"}`, 401},
		{"score without token", score, "", ev(eventAt(now)), 401},
		{"garbage token", score, "not-a-jwt", ev(eventAt(now)), 401},
		{"wrong secret", score, token(t, []byte("another-secret-that-is-32-bytes-long"), "alice", now), ev(eventAt(now)), 401},
		{"expired token", score, token(t, secret, "alice", now.Add(-time.Hour)), ev(eventAt(now)), 401},
		{"other user's team", score, token(t, secret, "bob", now), ev(eventAt(now)), 404},
		{"unknown team", "/teams/nope/score", alice, ev(eventAt(now)), 404},
		{"empty name", "/teams", alice, `{"name":""}`, 400},
		{"bad json", "/teams", alice, `{`, 400},
		{"bad json score", score, alice, `{`, 400},
		{"event_id not a uuid", score, alice, ev("e1"), 400},
		{"event_id v4", score, alice, ev(uuid.NewString()), 400},
		{"event_id too old", score, alice, ev(eventAt(now.Add(-EventRetention - time.Minute))), 400},
		{"event_id in the future", score, alice, ev(eventAt(now.Add(5 * time.Minute))), 400},
	}
	for _, c := range cases {
		if code, _ := post(t, srv.URL+c.path, c.tok, c.body); code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, code, c.want)
		}
	}
}

func TestDeleteEventsBefore(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	team, err := s.Create(ctx, "alice", "red", 0)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	recent := eventAt(now)
	for _, id := range []string{eventAt(now.Add(-72 * time.Hour)), recent} {
		if _, err := s.AddScore(ctx, "alice", team.ID, id, 1); err != nil {
			t.Fatal(err)
		}
	}

	n, err := s.DeleteEventsBefore(ctx, now.Add(-EventRetention))
	if err != nil || n != 1 {
		t.Fatalf("deleted %d (err %v), want 1", n, err)
	}
	var left string
	if err := s.db.Read.QueryRow(`SELECT event_id FROM score_events`).Scan(&left); err != nil || left != recent {
		t.Fatalf("remaining event %q (err %v), want %q", left, err, recent)
	}
}

func TestUUIDTimeRoundTrip(t *testing.T) {
	ts := time.UnixMilli(1_762_000_000_123)
	if got := uuidTime(uuidAt(ts)); !got.Equal(ts) {
		t.Fatalf("got %v, want %v", got, ts)
	}
}
