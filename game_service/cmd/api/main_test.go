package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestRunRequiresSecret(t *testing.T) {
	if err := run(context.Background(), env(map[string]string{"JWT_SECRET": "too-short"})); err == nil {
		t.Fatal("want error for short JWT_SECRET")
	}
}

func TestRunShutsDownGracefully(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, env(map[string]string{
			"PORT":       "0",
			"DB_PATH":    filepath.Join(t.TempDir(), "test.db"),
			"JWT_SECRET": "test-secret-that-is-at-least-32-bytes!",
		}))
	}()
	time.Sleep(100 * time.Millisecond) // let it start listening
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v, want nil after shutdown", err)
		}
	case <-time.After(shutdownTimeout):
		t.Fatal("run did not return after cancel")
	}
}

func TestCleanupEvery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := make(chan string, 100)
	job := func(name string, n int64, err error) func(context.Context) (int64, error) {
		return func(context.Context) (int64, error) { calls <- name; return n, err }
	}
	done := make(chan struct{})
	go func() {
		cleanupEvery(ctx, time.Millisecond, map[string]func(context.Context) (int64, error){
			"a": job("a", 3, nil),
			"b": job("b", 0, errors.New("boom")),
		})
		close(done)
	}()
	seen := map[string]bool{}
	for len(seen) < 2 {
		seen[<-calls] = true
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cleanupEvery did not stop after cancel")
	}
}
