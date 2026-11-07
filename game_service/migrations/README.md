# migrations

SQL migrations for the game_service database, run by [golang-migrate](https://github.com/golang-migrate/migrate).
One change per numbered pair:

```
001_init.up.sql
001_init.down.sql
002_<name>.up.sql
002_<name>.down.sql
```

- The files are embedded into the binary (`migrations.go`), and `db.Open` applies every pending `up` on startup.
  The applied version is stored in the `schema_migrations` table.
- Each file runs inside one transaction. If a migration fails, it rolls back, and the version is marked `dirty`
  and the app refuses to start until you fix it (`migrate ... force <version>`).
- Never edit a migration that has already been applied anywhere. Add a new one instead.

Rollback and status use the CLI (it needs the `sqlite` build tag, the pure-Go driver):

```
go install -tags sqlite github.com/golang-migrate/migrate/v4/cmd/migrate@v4.20.1
migrate -path migrations -database "sqlite://livelyboard.db" version
migrate -path migrations -database "sqlite://livelyboard.db" down 1
migrate create -ext sql -dir migrations -seq <name>   # new numbered up/down pair
```
