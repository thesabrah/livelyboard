package team

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"

	"livelyboard/internal/auth"
)

const (
	maxBody   = 1 << 10 // requests are tiny; cap them so a client can't stream junk at us
	clockSkew = time.Minute
)

func Routes(mux *http.ServeMux, s *Store, secret []byte) {
	mux.HandleFunc("POST /teams", auth.Require(secret, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name  string `json:"name"`
			Score int    `json:"score"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&in); err != nil || in.Name == "" {
			http.Error(w, "body must be JSON with a non-empty name", http.StatusBadRequest)
			return
		}
		t, err := s.Create(r.Context(), auth.UserID(r.Context()), in.Name, in.Score)
		if err != nil {
			log.Println("create team:", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, t)
	}))

	// Public: leaderboards are readable by anyone, and skipping the JWT check keeps reads cheap.
	mux.HandleFunc("GET /teams/{id}/rank", func(w http.ResponseWriter, r *http.Request) {
		st, err := s.Rank(r.Context(), r.PathValue("id"))
		if errors.Is(err, ErrNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if err != nil {
			log.Println("rank:", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, st)
	})

	mux.HandleFunc("GET /leaderboard", func(w http.ResponseWriter, r *http.Request) {
		top, err := s.Top(r.Context())
		if err != nil {
			log.Println("leaderboard:", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, top)
	})

	mux.HandleFunc("POST /teams/{id}/score", auth.Require(secret, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			EventID string `json:"event_id"`
			Delta   int    `json:"delta"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&in); err != nil {
			http.Error(w, "body must be JSON", http.StatusBadRequest)
			return
		}
		eventID, err := uuid.Parse(in.EventID)
		if err != nil || eventID.Version() != 7 {
			http.Error(w, "event_id must be a UUIDv7", http.StatusBadRequest)
			return
		}
		// Older events may already be cleaned up, so accepting them could double-count a retry.
		// Future ones would never be cleaned up.
		now := time.Now()
		if ts := uuidTime(eventID); ts.Before(now.Add(-EventRetention)) || ts.After(now.Add(clockSkew)) {
			http.Error(w, "event_id timestamp is outside the accepted window", http.StatusBadRequest)
			return
		}

		// eventID.String() normalizes to lowercase, which the text-ordered cleanup relies on.
		t, err := s.AddScore(r.Context(), auth.UserID(r.Context()), r.PathValue("id"), eventID.String(), in.Delta)
		if errors.Is(err, ErrNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if err != nil {
			log.Println("add score:", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, t)
	}))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
