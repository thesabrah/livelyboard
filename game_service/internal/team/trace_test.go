package team

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestDBSpans(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	s, ctx := newStore(t), context.Background()
	team, err := s.Create(ctx, "alice", "a", 0)
	if err != nil {
		t.Fatal(err)
	}
	ev := uuid.Must(uuid.NewV7()).String()
	for range 2 { // second call repeats the event: SELECT instead of UPDATE
		if _, err := s.AddScore(ctx, "alice", team.ID, ev, 1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Top(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rank(ctx, team.ID); err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, sp := range sr.Ended() {
		got = append(got, sp.Name())
		if sp.Name() == "db.wait_connection" { // waits for the pool, runs no SQL
			continue
		}
		attrs := attribute.NewSet(sp.Attributes()...)
		if sys, _ := attrs.Value("db.system.name"); sys.AsString() != "sqlite" {
			t.Errorf("%s: db.system.name = %q", sp.Name(), sys.AsString())
		}
		if q, _ := attrs.Value("db.query.text"); q.AsString() == "" {
			t.Errorf("%s: no db.query.text", sp.Name())
		}
	}
	want := []string{
		"db.wait_connection", "db.begin", "db.insert_event", "db.update_team", "db.commit",
		"db.wait_connection", "db.begin", "db.insert_event", "db.select_team", "db.commit",
		"db.query_leaderboard",
		"db.query_rank", "db.query_neighbors", "db.query_neighbors",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("spans = %v\nwant    %v", got, want)
	}
}
