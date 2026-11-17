# livelyboard

A real-time leaderboard built step by step toward **2 million concurrent clients**. Every step
is load-tested with k6 and measured with OpenTelemetry before the next design change.

This is a learning project. The goal is to understand *why* a leaderboard slows down at scale,
not only to ship one. Each step is written up as a blog post.

## Blog series (Arabic)

| Part | Post | What it covers |
|------|------|----------------|
| 1 | [من أول API لحد 5,000 VUs: بناء Real-Time Gaming Leaderboard](https://thesabrah.substack.com/p/api-5000-vus-real-time-gaming-leaderboard) | First version: Go + SQLite, idempotent score updates, load-tested up to 5,000 virtual users |
| 2 | [ازاي نعرف ايه اللي بيحصل جوه السيستم](https://thesabrah.substack.com/p/observability) | Observability: Four Golden Signals, OpenTelemetry metrics and traces, Grafana, and finding the bottleneck |

## What we've found so far

Both runs were on a personal machine, not a dedicated load-test server.

- **Part 1, at 5,000 VUs:** all 452,845 checks passed, yet p95 was 946 ms and p99 was 4.81 s.
  Reads were fine and writes were slow. Passing checks doesn't mean the performance targets were met.
- **Part 2, traced:** at about 2K req/s, p99 on `POST /teams/{id}/score` reached about 10 s. The traces
  showed that most of a slow request was spent in `db.wait_connection`, waiting for the single
  SQLite writer. SQLite allows one writer at a time, so every score update queues behind the
  others.
- **Next:** compare and try other
  designs, such as Redis sorted sets for ranking.

## Architecture (current)

```
clients ──HTTP──▶ game_service (Go, net/http)
                    ├── write pool: 1 connection  ─┐
                    └── read pool: 4×CPU, query_only ┴─▶ SQLite (WAL)
                    │
                    └── OTLP ──▶ OTel Collector ──▶ Prometheus (metrics)
                                                └─▶ Jaeger (traces) ──▶ Grafana
```

- **Score updates add a delta.** Each update carries a client-generated UUIDv7 `event_id`, so
  retries are safe: a repeated event is a no-op.
- **Rank** = 1 + the number of teams with a higher score (ties share a rank), read from a `(score, id)` index.
- **Auth:** 15-minute JWT access tokens and 30-day rotating refresh tokens. Teams belong to users.

### API

| Method | Path | Auth | Returns |
|--------|------|------|---------|
| POST | `/auth/register` | — | `{"username","password"}` |
| POST | `/auth/login` | — | `{access_token, refresh_token, expires_in}` |
| POST | `/auth/refresh` | — | new token pair (the old refresh token is revoked) |
| POST | `/auth/logout` | — | 204, revokes this session |
| POST | `/teams` | Bearer | creates a team `{"name","score"}` |
| POST | `/teams/{id}/score` | Bearer | adds `{"event_id","delta"}` to your team |
| GET | `/teams/{id}/rank` | public | `{id, name, score, rank, total_teams, above[2], below[2]}` |
| GET | `/leaderboard` | public | top 10 `[{rank, name, score}]` |

## Layout

```
livelyboard/
├── game_service/                 Go API on SQLite
│   ├── cmd/api/                  entry point, wiring, graceful shutdown
│   ├── internal/                 auth, db, team, telemetry
│   └── migrations/               SQL migrations (golang-migrate)
├── deployments/observability/    Collector, Prometheus, Jaeger, Grafana (docker compose)
└── loadtests/scenarios/          k6 scripts
```

## Run

```sh
cd game_service
JWT_SECRET=<at least 32 bytes> go run ./cmd/api     # listens on :8080
go test -race ./...                                  # coverage must stay ≥ 80%
```

Env: `PORT` (8080), `DB_PATH` (`livelyboard.db`), `JWT_SECRET` (required),
`OTEL_EXPORTER_OTLP_ENDPOINT` (optional; turns telemetry on).

## Observability

```sh
docker compose -f deployments/observability/docker-compose.yml up -d
cd game_service && OTEL_EXPORTER_OTLP_ENDPOINT=localhost:4317 OTEL_EXPORTER_OTLP_INSECURE=true \
  JWT_SECRET=<32+ bytes> go run ./cmd/api
```

- Grafana: http://localhost:3000. The livelyboard dashboard shows traffic, latency, errors,
  DB pool saturation, and the latest traces filtered by route.
- Jaeger: http://localhost:16686
- Prometheus: http://localhost:9090

## Load test

```sh
# terminal 1: server with a fresh DB
cd game_service && rm -f loadtest.db* && DB_PATH=loadtest.db JWT_SECRET=<32+ bytes> go run ./cmd/api
# terminal 2: from the repo root
k6 run loadtests/scenarios/score_rank.js
```

Scenarios: `basic` (20 VUs for 30 s), then `load` (ramps 0 → 500 VUs). Each VU makes about one request/s,
4 reads per score update. Thresholds: p95 < 500 ms, p99 < 1 s, < 1% failed.
Env: `BASE_URL`, `USERS`, `TEAMS`, `READ_RATIO`.
