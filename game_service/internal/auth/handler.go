package auth

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
)

const maxBody = 1 << 10

func Routes(mux *http.ServeMux, s *Store) {
	mux.HandleFunc("POST /auth/register", func(w http.ResponseWriter, r *http.Request) {
		var in credentials
		if !decode(w, r, &in) {
			return
		}
		// bcrypt only uses the first 72 bytes, so longer passwords would be silently truncated.
		if len(in.Username) < 3 || len(in.Username) > 32 || len(in.Password) < 8 || len(in.Password) > 72 {
			http.Error(w, "username must be 3-32 bytes and password 8-72 bytes", http.StatusBadRequest)
			return
		}
		u, err := s.Register(r.Context(), in.Username, in.Password)
		if errors.Is(err, ErrUsernameTaken) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		respond(w, http.StatusCreated, u, err)
	})

	mux.HandleFunc("POST /auth/login", func(w http.ResponseWriter, r *http.Request) {
		var in credentials
		if !decode(w, r, &in) {
			return
		}
		t, err := s.Login(r.Context(), in.Username, in.Password)
		if errors.Is(err, ErrBadCredentials) {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		respond(w, http.StatusOK, t, err)
	})

	mux.HandleFunc("POST /auth/refresh", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			RefreshToken string `json:"refresh_token"`
		}
		if !decode(w, r, &in) {
			return
		}
		t, err := s.Refresh(r.Context(), in.RefreshToken)
		if errors.Is(err, ErrBadRefresh) {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		respond(w, http.StatusOK, t, err)
	})

	mux.HandleFunc("POST /auth/logout", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			RefreshToken string `json:"refresh_token"`
		}
		if !decode(w, r, &in) {
			return
		}
		if err := s.Logout(r.Context(), in.RefreshToken); err != nil {
			respond(w, 0, nil, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(v); err != nil {
		http.Error(w, "body must be JSON", http.StatusBadRequest)
		return false
	}
	return true
}

func respond(w http.ResponseWriter, status int, v any, err error) {
	if err != nil {
		log.Println("auth:", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
