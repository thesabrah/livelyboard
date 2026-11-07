# livelyboard

A learning project: a real-time leaderboard where many clients submit scores and read rankings,
built step by step toward 2 million concurrent clients, with load tests and observability.

## Layout

```
livelyboard/
├── game_service/            Go API (auth, teams, scores, ranks) on SQLite
│   ├── cmd/api/             entry point
│   ├── internal/            auth, db, team
│   └── migrations/          SQL migrations
└── loadtests/
    └── scenarios/           k6 scripts
```

Added when there is something to put in them: `docs/` (architecture, decisions, experiments,
benchmarks), `deployments/observability/`, `loadtests/results/`.

## Run

```sh
cd game_service
JWT_SECRET=<at least 32 bytes> go run ./cmd/api     # listens on :8080
go test -race ./...
```

Env: `PORT` (8080), `DB_PATH` (`livelyboard.db`), `JWT_SECRET` (required).

## Load test

```sh
# terminal 1: server with a fresh DB
cd game_service && rm -f loadtest.db* && DB_PATH=loadtest.db JWT_SECRET=<32+ bytes> go run ./cmd/api
# terminal 2: from the repo root
k6 run loadtests/scenarios/score_rank.js
```
