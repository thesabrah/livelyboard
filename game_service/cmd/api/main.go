package main

import (
	"cmp"
	"context"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"livelyboard/internal/auth"
	"livelyboard/internal/db"
	"livelyboard/internal/team"

	"github.com/joho/godotenv"
)

const shutdownTimeout = 10 * time.Second // fits inside Kubernetes' default 30s grace period

func main() {
	// .env is optional: in Docker/Kubernetes env vars are set directly and there is no file.
	if err := godotenv.Load(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Fatalf("Error loading .env file: %v", err)
	}

	log.Println("Starting game service...")
	if err := run(context.Background(), os.Getenv); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, getenv func(string) string) error {
	port := cmp.Or(getenv("PORT"), "8080")
	dbPath := cmp.Or(getenv("DB_PATH"), "livelyboard.db")
	secret := []byte(getenv("JWT_SECRET"))
	if len(secret) < 32 {
		return errors.New("JWT_SECRET must be set and at least 32 bytes")
	}

	conn, err := db.Open(dbPath)
	if err != nil {
		return err
	}
	defer conn.Close() // runs last: after HTTP has drained and cleanup has stopped
	teams, err := team.New(conn)
	if err != nil {
		return err
	}
	users, err := auth.New(conn, secret)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	team.Routes(mux, teams, secret)
	auth.Routes(mux, users)

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	wg.Go(func() {
		cleanupEvery(ctx, time.Hour, map[string]func(context.Context) (int64, error){
			"old score events": func(ctx context.Context) (int64, error) {
				return teams.DeleteEventsBefore(ctx, time.Now().Add(-team.EventRetention))
			},
			"expired refresh tokens": func(ctx context.Context) (int64, error) {
				return users.DeleteExpiredRefreshTokens(ctx, time.Now())
			},
		})
	})

	srv := &http.Server{Addr: ":" + port, Handler: mux,ReadHeaderTimeout: 5 * time.Second}
	errCh := make(chan error, 1)
	go func() {
		log.Println("Listening on port", port)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err = <-errCh: // failed to start, e.g. port already in use
	case <-ctx.Done():
		stop() // restore default signal handling: a second Ctrl+C kills immediately
		log.Println("Shutting down: draining in-flight requests...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		err = srv.Shutdown(shutdownCtx)
	}
	stop() // also cancels ctx on the startup-failure path, so cleanup exits
	wg.Wait()
	log.Println("Stopped")
	return err
}

// cleanupEvery runs every job now and then on each tick, until ctx is cancelled.
func cleanupEvery(ctx context.Context, every time.Duration, jobs map[string]func(context.Context) (int64, error)) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		for name, job := range jobs {
			if n, err := job(ctx); err != nil && ctx.Err() == nil {
				log.Printf("cleanup %s: %v", name, err)
			} else if n > 0 {
				log.Printf("cleanup: deleted %d %s", n, name)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
