package db

import (
	"context"
	"database/sql"
	"errors"
	"runtime"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "modernc.org/sqlite"

	"livelyboard/migrations"
)

// DB is two pools on one SQLite file. SQLite allows one writer at a time, so with a single
// write connection writes queue fairly in Go (bounded by each request's context) instead of
// polling SQLite's lock, which is unfair under load and fails with SQLITE_BUSY after busy_timeout.
type DB struct {
	Write *sql.DB // exactly one connection; every INSERT/UPDATE/DELETE and write tx goes here
	Read  *sql.DB // many read-only connections; WAL lets them run alongside the writer
}

// WAL: readers don't block the writer.
// synchronous=NORMAL: in WAL mode, fsync only at checkpoints, not on every commit (~17x more
// writes/s on this disk). An app crash loses nothing and the DB can't corrupt, but a power cut
// or OS crash can roll back the last few commits.
// busy_timeout: still needed for the rare waits on SQLite's own locks (e.g. checkpoints).
// _txlock=immediate: take the write lock at BEGIN, so a tx never fails on a read->write upgrade.
const pragmas = "_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"

// Open opens (or creates) the SQLite file at path and applies any pending migrations.
func Open(path string) (*DB, error) {
	w, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_txlock=immediate&"+pragmas)
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	// query_only: a write sent to the read pool by mistake fails loudly instead of competing for the lock.
	r, err := sql.Open("sqlite", "file:"+path+"?"+pragmas+"&_pragma=query_only(1)")
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(4 * runtime.NumCPU()) // enough to keep CPUs busy; unbounded would let a stress test open thousands
	d := &DB{Write: w, Read: r}
	if err := migrateUp(w); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// migrateUp applies every migration in migrations.FS not yet recorded in schema_migrations.
func migrateUp(w *sql.DB) error {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return err
	}
	drv, err := sqlite.WithInstance(w, &sqlite.Config{})
	if err != nil {
		return err
	}
	m, err := migrate.NewWithInstance("iofs", src, "sqlite", drv)
	if err != nil {
		return err
	}
	// No m.Close(): it would close w, which the app keeps using.
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}

func (d *DB) Close() error { return errors.Join(d.Read.Close(), d.Write.Close()) }

// DeleteInBatches runs a DELETE whose last placeholder is a "LIMIT ?" (batch is appended to args)
// until a run deletes fewer than batch rows. Small batches release the single writer
// between runs, so live requests aren't blocked behind one huge delete.
func DeleteInBatches(ctx context.Context, conn *sql.DB, batch int, query string, args ...any) (int64, error) {
	args = append(args, batch)
	var total int64
	for {
		res, err := conn.ExecContext(ctx, query, args...)
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
		if n < int64(batch) {
			return total, nil
		}
	}
}
