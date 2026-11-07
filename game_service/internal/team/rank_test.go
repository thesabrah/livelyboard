package team

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRank(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	ids := map[string]string{}
	for _, tm := range []struct {
		name  string
		score int
	}{{"gold", 50}, {"red", 40}, {"blue", 40}, {"green", 30}} {
		team, err := s.Create(ctx, "alice", tm.name, tm.score)
		if err != nil {
			t.Fatal(err)
		}
		ids[tm.name] = team.ID
	}
	mux := http.NewServeMux()
	Routes(mux, s, secret)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	get := func(id string) (int, Standing) {
		resp, err := http.Get(srv.URL + "/teams/" + id + "/rank") // no token: the route is public
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var st Standing
		json.NewDecoder(resp.Body).Decode(&st)
		return resp.StatusCode, st
	}
	names := func(rs []Ranked) string {
		var out []string
		for _, r := range rs {
			out = append(out, r.Name+":"+string(rune('0'+r.Rank)))
		}
		return strings.Join(out, ",")
	}

	wantRank := map[string]int{"gold": 1, "red": 2, "blue": 2, "green": 4}
	for name, want := range wantRank {
		code, st := get(ids[name])
		if code != 200 || st.Rank != want || st.TotalTeams != 4 || st.Name != name {
			t.Errorf("%s: got %d %+v, want rank %d of 4", name, code, st.Ranked, want)
		}
	}

	// Tied teams are ordered by id DESC, so the newer of red/blue (blue) is placed first.
	_, st := get(ids["blue"])
	if got := names(st.Above); got != "gold:1" {
		t.Errorf("blue above = %s, want gold:1", got)
	}
	if got := names(st.Below); got != "red:2,green:4" {
		t.Errorf("blue below = %s, want red:2,green:4", got)
	}
	if _, st := get(ids["gold"]); len(st.Above) != 0 || st.Above == nil {
		t.Errorf("top team above = %#v, want empty list", st.Above)
	}
	if _, st := get(ids["green"]); len(st.Below) != 0 || st.Below == nil {
		t.Errorf("last team below = %#v, want empty list", st.Below)
	}
	if code, _ := get("nope"); code != 404 {
		t.Errorf("unknown team: got %d, want 404", code)
	}
}

func TestTop(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mux := http.NewServeMux()
	Routes(mux, s, secret)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	get := func() string {
		resp, err := http.Get(srv.URL + "/leaderboard") // no token: the route is public
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		var top []Leader
		json.NewDecoder(resp.Body).Decode(&top)
		var out []string
		for _, l := range top {
			out = append(out, fmt.Sprintf("%d:%s:%d", l.Rank, l.Name, l.Score))
		}
		return strings.Join(out, ",")
	}

	if got := get(); got != "" {
		t.Errorf("empty leaderboard = %q, want []", got)
	}
	// 12 teams: only the best 10 come back. blue ties red and is newer, so it comes first.
	for _, tm := range []struct {
		name  string
		score int
	}{{"gold", 50}, {"red", 40}, {"blue", 40}, {"green", 30}, {"f0", 0}, {"f1", 1}, {"f2", 2}, {"f3", 3}, {"f4", 4}, {"f5", 5}, {"f6", 6}, {"f7", 7}} {
		if _, err := s.Create(ctx, "alice", tm.name, tm.score); err != nil {
			t.Fatal(err)
		}
	}
	want := "1:gold:50,2:blue:40,3:red:40,4:green:30,5:f7:7,6:f6:6,7:f5:5,8:f4:4,9:f3:3,10:f2:2"
	if got := get(); got != want {
		t.Errorf("leaderboard =\n  %s\nwant\n  %s", got, want)
	}
}

// Guards against a change that silently turns rank lookups into full table scans.
func TestRankQueriesUseScoreIndex(t *testing.T) {
	s := newStore(t)
	for _, q := range []string{
		`SELECT COUNT(*) FROM teams t2 WHERE t2.score > 10`,
		rankedSelect + `WHERE (score, id) > (10, 'x') ORDER BY score, id LIMIT 2`,
		rankedSelect + `WHERE (score, id) < (10, 'x') ORDER BY score DESC, id DESC LIMIT 2`,
	} {
		rows, err := s.db.Read.Query(`EXPLAIN QUERY PLAN ` + q)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			rows.Scan(&id, &parent, &unused, &detail)
			plan = append(plan, detail)
		}
		rows.Close()
		joined := strings.Join(plan, " | ")
		if !strings.Contains(joined, "teams_score") || strings.Contains(joined, "SCAN t ") || strings.HasSuffix(joined, "SCAN t") {
			t.Errorf("query does not use teams_score range search:\n  %s\n  plan: %s", q, joined)
		}
	}

	// The top query scans the index backwards on purpose; what must not happen is a sort of the whole table.
	rows, err := s.db.Read.Query(`EXPLAIN QUERY PLAN `+topQuery, topN)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		rows.Scan(&id, &parent, &unused, &detail)
		plan = append(plan, detail)
	}
	if joined := strings.Join(plan, " | "); !strings.Contains(joined, "teams_score") || strings.Contains(joined, "TEMP B-TREE") {
		t.Errorf("top query does not walk teams_score in order:\n  plan: %s", joined)
	}
}
