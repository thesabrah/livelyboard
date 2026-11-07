package db

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func open(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if _, err := d.Write.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, old INTEGER)`); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	for range 2 { // second Open must find nothing to apply and still succeed
		d, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		var version, dirty int
		if err := d.Read.QueryRow(`SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil || version != 1 || dirty != 0 {
			t.Fatalf("version=%d dirty=%d (err %v), want 1 clean", version, dirty, err)
		}
		for _, table := range []string{"users", "refresh_tokens", "teams", "score_events"} {
			if _, err := d.Read.Exec(`SELECT 1 FROM ` + table + ` LIMIT 1`); err != nil {
				t.Errorf("table %s: %v", table, err)
			}
		}
		d.Close()
	}

	if _, err := Open(filepath.Join(t.TempDir(), "missing-dir", "x.db")); err == nil {
		t.Fatal("want error when the DB file can't be created")
	}
}

func TestPools(t *testing.T) {
	d := open(t)
	if n := d.Write.Stats().MaxOpenConnections; n != 1 {
		t.Fatalf("write pool max conns = %d, want 1", n)
	}
	for name, want := range map[string]string{"journal_mode": "wal", "synchronous": "1"} { // 1 = NORMAL
		var got string
		if err := d.Read.QueryRow("PRAGMA " + name).Scan(&got); err != nil || got != want {
			t.Errorf("PRAGMA %s = %q (err %v), want %q", name, got, err, want)
		}
	}
	if _, err := d.Read.Exec(`INSERT INTO t (old) VALUES (1)`); err == nil {
		t.Fatal("read pool accepted a write; want query_only error")
	}
}

// Regression for SQLITE_BUSY under load: many concurrent writers must all succeed,
// because they queue for the single write connection instead of polling SQLite's lock.
func TestConcurrentWritesNeverBusy(t *testing.T) {
	d := open(t)
	var failed atomic.Int64
	var wg sync.WaitGroup
	for range 200 {
		wg.Go(func() {
			for range 10 {
				if _, err := d.Write.Exec(`INSERT INTO t (old) VALUES (0)`); err != nil {
					failed.Add(1)
				}
			}
		})
	}
	wg.Wait()
	var n int
	d.Read.QueryRow(`SELECT count(*) FROM t`).Scan(&n)
	if failed.Load() != 0 || n != 2000 {
		t.Fatalf("failed=%d rows=%d, want 0 failed and 2000 rows", failed.Load(), n)
	}
}

func TestDeleteInBatches(t *testing.T) {
	d := open(t)
	// 25 old rows (3 batches of 10) and 1 that must survive
	if _, err := d.Write.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 25)
		INSERT INTO t (old) SELECT 1 FROM n`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write.Exec(`INSERT INTO t (old) VALUES (0)`); err != nil {
		t.Fatal(err)
	}

	n, err := DeleteInBatches(context.Background(), d.Write, 10,
		`DELETE FROM t WHERE id IN (SELECT id FROM t WHERE old = ? LIMIT ?)`, 1)
	if err != nil || n != 25 {
		t.Fatalf("deleted %d (err %v), want 25", n, err)
	}
	var left int
	d.Read.QueryRow(`SELECT count(*) FROM t`).Scan(&left)
	if left != 1 {
		t.Fatalf("%d rows left, want 1", left)
	}

	if _, err := DeleteInBatches(context.Background(), d.Write, 10, `DELETE FROM missing LIMIT ?`); err == nil {
		t.Fatal("want error for bad query")
	}
}
