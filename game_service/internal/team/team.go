package team

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"

	"livelyboard/internal/db"
)

// EventRetention is how long score events are kept for retry dedupe.
// The API rejects events older than this, so a retry can never arrive after its event was cleaned up.
const EventRetention = 48 * time.Hour

const deleteBatch = 1000

type Team struct {
	ID      string `json:"id"`
	OwnerID string `json:"owner_id"`
	Name    string `json:"name"`
	Score   int    `json:"score"`
}

var ErrNotFound = errors.New("team not found")

type Store struct{ db *db.DB }

// No-op until telemetry.Setup installs a provider.
var tracer = otel.Tracer("livelyboard/team")

// endSpan records err, if any, and ends the span.
func endSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

// startQuery starts a span for one SQL statement (placeholders only, never values).
func startQuery(ctx context.Context, name, query string, attrs ...attribute.KeyValue) trace.Span {
	attrs = append(attrs, semconv.DBSystemNameSQLite, semconv.DBQueryText(query))
	_, span := tracer.Start(ctx, name, trace.WithAttributes(attrs...))
	return span
}

// New expects the schema from migrations/ (applied by db.Open).
func New(conn *db.DB) (*Store, error) {
	return &Store{db: conn}, nil
}

func (s *Store) Create(ctx context.Context, ownerID, name string, score int) (Team, error) {
	id, err := uuid.NewV7() // time-ordered, so inserts append to the end of the B-tree
	if err != nil {
		return Team{}, err
	}
	t := Team{ID: id.String(), OwnerID: ownerID, Name: name, Score: score}
	_, err = s.db.Write.ExecContext(ctx, `INSERT INTO teams (id, owner_id, name, score) VALUES (?, ?, ?, ?)`,
		t.ID, t.OwnerID, t.Name, t.Score)
	return t, err
}

const (
	insertEventQuery = `INSERT INTO score_events (event_id, team_id, delta) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`
	updateTeamQuery  = `UPDATE teams SET score = score + ? WHERE id = ? AND owner_id = ? RETURNING id, owner_id, name, score`
	selectTeamQuery  = `SELECT id, owner_id, name, score FROM teams WHERE id = ? AND owner_id = ?`
)

// AddScore adds delta to a team owned by ownerID, once per eventID; a repeated eventID is a no-op
// that returns the current team, so client retries are safe. A team owned by someone else is ErrNotFound.
func (s *Store) AddScore(ctx context.Context, ownerID, teamID, eventID string, delta int) (Team, error) {
	// One span per step; pool wait and SQLite lock wait (BEGIN) are split to tell them apart.
	_, span := tracer.Start(ctx, "db.wait_connection")
	conn, err := s.db.Write.Conn(ctx)
	endSpan(span, err)
	if err != nil {
		return Team{}, err
	}
	defer conn.Close() // runs after tx.Rollback

	span = startQuery(ctx, "db.begin", "BEGIN IMMEDIATE") // _txlock=immediate
	tx, err := conn.BeginTx(ctx, nil)
	endSpan(span, err)
	if err != nil {
		return Team{}, err
	}
	defer tx.Rollback()

	span = startQuery(ctx, "db.insert_event", insertEventQuery)
	var inserted int64
	res, err := tx.ExecContext(ctx, insertEventQuery, eventID, teamID, delta)
	if err == nil {
		inserted, err = res.RowsAffected()
	}
	endSpan(span, err)
	if err != nil {
		return Team{}, err
	}

	// QueryRowContext runs the query immediately, so pick exactly one.
	var row *sql.Row
	if inserted == 1 {
		span = startQuery(ctx, "db.update_team", updateTeamQuery)
		row = tx.QueryRowContext(ctx, updateTeamQuery, delta, teamID, ownerID)
	} else {
		span = startQuery(ctx, "db.select_team", selectTeamQuery) // repeated event_id
		row = tx.QueryRowContext(ctx, selectTeamQuery, teamID, ownerID)
	}
	var t Team
	err = row.Scan(&t.ID, &t.OwnerID, &t.Name, &t.Score)
	if errors.Is(err, sql.ErrNoRows) {
		endSpan(span, nil)         // not found isn't a failure
		return Team{}, ErrNotFound // rollback also drops the event row
	}
	endSpan(span, err)
	if err != nil {
		return Team{}, err
	}

	span = startQuery(ctx, "db.commit", "COMMIT")
	err = tx.Commit()
	endSpan(span, err)
	return t, err
}

// Ranked is a team's public leaderboard entry. Ties share a rank (50, 40, 40, 30 -> 1, 2, 2, 4).
type Ranked struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Score int    `json:"score"`
	Rank  int    `json:"rank"`
}

type Standing struct {
	Ranked
	TotalTeams int      `json:"total_teams"`
	Above      []Ranked `json:"above"` // closest neighbors with a better position, best first
	Below      []Ranked `json:"below"` // closest neighbors with a worse position, best first
}

const neighbors = 2

// rank = 1 + teams with a higher score. Walks the score index, so cost grows with the rank: O(rank).
const rankedSelect = `SELECT id, name, score, (SELECT COUNT(*) FROM teams t2 WHERE t2.score > t.score) + 1 FROM teams t `

// shortcut: COUNT(*) per request is O(n); keep a counter or move to Redis ZCARD when teams grow.
const rankQuery = `SELECT id, name, score,
	(SELECT COUNT(*) FROM teams t2 WHERE t2.score > t.score) + 1,
	(SELECT COUNT(*) FROM teams)
	FROM teams t WHERE id = ?`

// Rank returns a team's rank, the total team count and its nearest neighbors.
// Neighbors use the total order (score DESC, id DESC); the order inside a tie is arbitrary.
// The queries are not in one transaction (reads use the read pool, without a snapshot tx), so a
// concurrent score update can make neighbor ranks momentarily inconsistent.
func (s *Store) Rank(ctx context.Context, id string) (Standing, error) {
	var st Standing
	span := startQuery(ctx, "db.query_rank", rankQuery)
	err := s.db.Read.QueryRowContext(ctx, rankQuery, id).Scan(&st.ID, &st.Name, &st.Score, &st.Rank, &st.TotalTeams)
	if errors.Is(err, sql.ErrNoRows) {
		endSpan(span, nil) // not found isn't a failure
		return Standing{}, ErrNotFound
	}
	endSpan(span, err)
	if err != nil {
		return Standing{}, err
	}
	if st.Above, err = s.ranked(ctx, "above", `WHERE (score, id) > (?, ?) ORDER BY score, id LIMIT ?`, st.Score, st.ID); err != nil {
		return Standing{}, err
	}
	slices.Reverse(st.Above) // fetched closest-first; return best first
	if st.Below, err = s.ranked(ctx, "below", `WHERE (score, id) < (?, ?) ORDER BY score DESC, id DESC LIMIT ?`, st.Score, st.ID); err != nil {
		return Standing{}, err
	}
	return st, nil
}

func (s *Store) ranked(ctx context.Context, direction, where string, score int, id string) (out []Ranked, err error) {
	span := startQuery(ctx, "db.query_neighbors", rankedSelect+where, attribute.String("direction", direction))
	defer func() { endSpan(span, err) }()
	rows, err := s.db.Read.QueryContext(ctx, rankedSelect+where, score, id, neighbors)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out = []Ranked{} // non-nil so JSON is [] rather than null
	for rows.Next() {
		var r Ranked
		if err := rows.Scan(&r.ID, &r.Name, &r.Score, &r.Rank); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Leader is one row of the top-10 leaderboard. Rank is the position in the list (1..10):
// tied teams get different positions, ordered by id DESC like Rank's neighbors.
type Leader struct {
	Rank  int    `json:"rank"`
	Name  string `json:"name"`
	Score int    `json:"score"`
}

const topN = 10

// Reads the teams_score index backwards and stops after topN rows, so cost doesn't grow with team count.
const topQuery = `SELECT name, score FROM teams ORDER BY score DESC, id DESC LIMIT ?`

// Top returns the best topN teams, best first.
func (s *Store) Top(ctx context.Context) (out []Leader, err error) {
	span := startQuery(ctx, "db.query_leaderboard", topQuery)
	defer func() { endSpan(span, err) }()
	rows, err := s.db.Read.QueryContext(ctx, topQuery, topN)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out = []Leader{} // non-nil so JSON is [] rather than null
	for rows.Next() {
		l := Leader{Rank: len(out) + 1}
		if err := rows.Scan(&l.Name, &l.Score); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// DeleteEventsBefore removes score events whose UUIDv7 timestamp is before cutoff, in batches.
func (s *Store) DeleteEventsBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	return db.DeleteInBatches(ctx, s.db.Write, deleteBatch, `DELETE FROM score_events WHERE event_id IN
		(SELECT event_id FROM score_events WHERE event_id < ? LIMIT ?)`, uuidAt(cutoff).String())
}

// uuidAt returns the smallest UUID whose first 48 bits are t's unix ms timestamp,
// so `event_id < uuidAt(t)` (as lowercase text) means "event created before t".
func uuidAt(t time.Time) uuid.UUID {
	var u uuid.UUID
	ms := t.UnixMilli()
	for i := range 6 {
		u[i] = byte(ms >> (40 - 8*i))
	}
	return u
}

// uuidTime reads the unix ms timestamp from the first 48 bits of a UUIDv7.
func uuidTime(u uuid.UUID) time.Time {
	var ms int64
	for i := range 6 {
		ms = ms<<8 | int64(u[i])
	}
	return time.UnixMilli(ms)
}
