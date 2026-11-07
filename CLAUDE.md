# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

@AGENTS.md

## Current state

- Repo layout: `game_service/` (Go service; `migrations/` = SQL migrations), `loadtests/scenarios/` (k6 scripts), `README.md`. Planned, created when their first file is written: `docs/{architecture,decisions,experiments,benchmarks}/`, `deployments/observability/`, `loadtests/results/`.
- `game_service/`: Go (module `livelyboard`), stdlib `net/http`, SQLite via `modernc.org/sqlite` (pure Go, no cgo), IDs via `github.com/google/uuid` v7, JWT via `golang-jwt/jwt/v5`, bcrypt via `golang.org/x/crypto`.
  - Packages: `internal/db` (opens SQLite as two pools: `Write` = 1 connection for all writes/write txs, `Read` = 4×CPU query_only connections; WAL, synchronous=NORMAL, busy_timeout, immediate tx). New write code must use `.Write`, reads `.Read`, `internal/auth` (users, login, tokens, `Require` middleware), `internal/team` (teams, scores, event cleanup), `cmd/api` (`run()` wiring + graceful shutdown).
  - Auth: `POST /auth/register` `{"username","password"}`, `POST /auth/login` → `{access_token, refresh_token, expires_in}`, `POST /auth/refresh` `{"refresh_token"}` (rotates), `POST /auth/logout` `{"refresh_token"}` → 204 (revokes this session only; the access JWT stays valid until its 15 min expiry, by design). Access = HS256 JWT, 15 min, `sub` = user id. Refresh = opaque, 30 days, stored as SHA-256 hash.
  - `POST /teams` `{"name","score"}` and `POST /teams/{id}/score` `{"event_id","delta"}` need `Authorization: Bearer <access_token>`. Teams have an `owner_id`; someone else's team returns 404.
  - `GET /teams/{id}/rank` is public → `{id, name, score, rank, total_teams, above[2], below[2]}`. Rank = 1 + teams with a higher score (ties share a rank). Uses index `teams_score` (holds (score, id)); cost is O(rank) per count, `total_teams` is O(n). `TestRankQueriesUseScoreIndex` guards the query plans.
  - `GET /leaderboard` is public → fixed top 10 `[{rank, name, score}]`, best first. `rank` is the position 1..10 (ties get different positions, ordered by id DESC), counted in Go. Reads `teams_score` backwards and stops at 10 rows, so cost doesn't grow with team count. Empty DB → `[]`.
  - Score updates are ADD (delta). `event_id` is the idempotency key and must be a client-generated UUIDv7 whose timestamp is within [now−48h, now+1min]. One hourly loop in `cmd/api` (`cleanupEvery`) deletes events older than 48h and expired refresh tokens, in batches of 1000 (`db.DeleteInBatches`).
  - SIGINT/SIGTERM: stop accepting, drain in-flight requests (10s), stop cleanup, then close DB. Second signal kills immediately.
  - Env: `PORT` (default 8080), `DB_PATH` (default `livelyboard.db`), `JWT_SECRET` (required, ≥32 bytes).
  - Run: `cd game_service && JWT_SECRET=<32+ bytes> go run ./cmd/api`. Test: `cd game_service && go test -race ./...`. Coverage: `go test -coverprofile=cover.out ./... && go tool cover -func=cover.out | tail -1` (must stay ≥ 80%).
  - Load test (k6): start the server with a fresh DB (`rm -f loadtest.db*; DB_PATH=loadtest.db JWT_SECRET=<32+ bytes> go run ./cmd/api`), then from the repo root `k6 run loadtests/scenarios/score_rank.js`. Scenarios: `basic` (20 VUs, 30s) then `load` (ramping-vus 0→500 over 1m, hold 1m, down 30s). Each VU = one user: one request per loop (`READ_RATIO` reads per score update, default 4; reads split 50/50 between `/teams/{id}/rank` and `/leaderboard`) + `sleep(1)`, so 500 users ≈ 500 req/s. Closed model: if `http_reqs`/s drops below the VU count, the server is the limit. Thresholds: p95<500ms, p99<1s, <1% failed (global, per endpoint, per scenario); summary prints p99. Env: `BASE_URL`, `USERS`, `TEAMS` (10000), `READ_RATIO`.
  - Schema lives only in `game_service/migrations/` (golang-migrate v4, numbered `NNN_name.up/down.sql`, embedded via `migrations.go`). `db.Open` applies pending ups on the Write pool; version in `schema_migrations`. Schema change = new migration file, never edit an applied one. Rollback/status via the `migrate` CLI (see `migrations/README.md`). `TestMigrations` guards it.
- Decided: Go, SQLite for now (single writer, single machine; known ceiling), ADD score updates, UUIDv7 stored as TEXT, user accounts owning teams, access + refresh tokens, golang-migrate for schema migrations. Still open: transport, leaderboard reads/ranking, sharding, observability.
- Not a git repository yet.

## How to work here

- This is a learning project. The user wants to understand how a real-time leaderboard works at scale (2M concurrent clients), not just get a finished system.
- For design decisions still open (see Current state), present options with trade-offs. Don't choose for the user.
- Before writing any code, describe what you plan to write, why, and what follows from it, then wait for approval. This also applies to small changes and scaffolding.
- Once the design is settled, record the architecture, run, test and load-test commands in this file.
- make the test coverage don't allowed to be down than 80%. 
