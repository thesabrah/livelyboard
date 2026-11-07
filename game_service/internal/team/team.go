package team

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"

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

// AddScore adds delta to a team owned by ownerID, once per eventID; a repeated eventID is a no-op
// that returns the current team, so client retries are safe. A team owned by someone else is ErrNotFound.
	func (s *Store) AddScore(ctx context.Context, ownerID, teamID, eventID string, delta int) (Team, error) {
		tx, err := s.db.Write.BeginTx(ctx, nil)
		if err != nil {
			return Team{}, err
		}
		defer tx.Rollback()

		res, err := tx.ExecContext(ctx,
			`INSERT INTO score_events (event_id, team_id, delta) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`,
			eventID, teamID, delta)
		if err != nil {
			return Team{}, err
		}
		inserted, err := res.RowsAffected()
		if err != nil {
			return Team{}, err
		}

		row := tx.QueryRowContext(ctx, `SELECT id, owner_id, name, score FROM teams WHERE id = ? AND owner_id = ?`,
			teamID, ownerID)
		if inserted == 1 {
			row = tx.QueryRowContext(ctx,
				`UPDATE teams SET score = score + ? WHERE id = ? AND owner_id = ? RETURNING id, owner_id, name, score`,
				delta, teamID, ownerID)
		}
		var t Team
		if err := row.Scan(&t.ID, &t.OwnerID, &t.Name, &t.Score); errors.Is(err, sql.ErrNoRows) {
			return Team{}, ErrNotFound // rollback also drops the event row
		} else if err != nil {
			return Team{}, err
		}
		return t, tx.Commit()
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

// Rank returns a team's rank, the total team count and its nearest neighbors.
// Neighbors use the total order (score DESC, id DESC); the order inside a tie is arbitrary.
// The queries are not in one transaction (reads use the read pool, without a snapshot tx), so a
// concurrent score update can make neighbor ranks momentarily inconsistent.
func (s *Store) Rank(ctx context.Context, id string) (Standing, error) {
	var st Standing
	// shortcut: COUNT(*) per request is O(n); keep a counter or move to Redis ZCARD when teams grow.
	err := s.db.Read.QueryRowContext(ctx, `SELECT id, name, score,
		(SELECT COUNT(*) FROM teams t2 WHERE t2.score > t.score) + 1,
		(SELECT COUNT(*) FROM teams)
		FROM teams t WHERE id = ?`, id).Scan(&st.ID, &st.Name, &st.Score, &st.Rank, &st.TotalTeams)
	if errors.Is(err, sql.ErrNoRows) {
		return Standing{}, ErrNotFound
	}
	if err != nil {
		return Standing{}, err
	}
	if st.Above, err = s.ranked(ctx, `WHERE (score, id) > (?, ?) ORDER BY score, id LIMIT ?`, st.Score, st.ID); err != nil {
		return Standing{}, err
	}
	slices.Reverse(st.Above) // fetched closest-first; return best first
	if st.Below, err = s.ranked(ctx, `WHERE (score, id) < (?, ?) ORDER BY score DESC, id DESC LIMIT ?`, st.Score, st.ID); err != nil {
		return Standing{}, err
	}
	return st, nil
}

func (s *Store) ranked(ctx context.Context, where string, score int, id string) ([]Ranked, error) {
	rows, err := s.db.Read.QueryContext(ctx, rankedSelect+where, score, id, neighbors)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Ranked{} // non-nil so JSON is [] rather than null
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
func (s *Store) Top(ctx context.Context) ([]Leader, error) {
	rows, err := s.db.Read.QueryContext(ctx, topQuery, topN)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Leader{} // non-nil so JSON is [] rather than null
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
