-- IF NOT EXISTS: lets a dev DB created before migrations existed adopt this version without errors.

CREATE TABLE IF NOT EXISTS users (
	id            TEXT PRIMARY KEY,
	username      TEXT NOT NULL UNIQUE,
	password_hash TEXT NOT NULL
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS refresh_tokens (
	token_hash TEXT PRIMARY KEY,
	user_id    TEXT NOT NULL,
	expires_at INTEGER NOT NULL
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS refresh_tokens_expires ON refresh_tokens(expires_at);

CREATE TABLE IF NOT EXISTS teams (
	id       TEXT PRIMARY KEY,
	owner_id TEXT NOT NULL,
	name     TEXT NOT NULL,
	score    INTEGER NOT NULL
) WITHOUT ROWID;
-- WITHOUT ROWID: this index holds (score, id), the exact order rank and neighbor queries walk.
CREATE INDEX IF NOT EXISTS teams_score ON teams(score);

CREATE TABLE IF NOT EXISTS score_events (
	event_id TEXT PRIMARY KEY,
	team_id  TEXT NOT NULL,
	delta    INTEGER NOT NULL
) WITHOUT ROWID;
