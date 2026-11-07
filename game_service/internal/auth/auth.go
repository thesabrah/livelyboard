package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"livelyboard/internal/db"
)

const (
	AccessTTL  = 15 * time.Minute
	RefreshTTL = 30 * 24 * time.Hour
)

var (
	ErrUsernameTaken  = errors.New("username already taken")
	ErrBadCredentials = errors.New("invalid username or password")
	ErrBadRefresh     = errors.New("invalid or expired refresh token")
)

const deleteBatch = 1000

// dummyHash is checked when the username doesn't exist, so login takes the same time either way
// and response timing doesn't reveal which usernames are registered.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy-password"), bcrypt.DefaultCost)

type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

type Tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"` // access token lifetime in seconds
}

type Store struct {
	db     *db.DB
	secret []byte
}

// New expects the schema from migrations/ (applied by db.Open).
func New(conn *db.DB, secret []byte) (*Store, error) {
	return &Store{db: conn, secret: secret}, nil
}

func (s *Store) Register(ctx context.Context, username, password string) (User, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return User{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return User{}, err
	}
	res, err := s.db.Write.ExecContext(ctx,
		`INSERT INTO users (id, username, password_hash) VALUES (?, ?, ?) ON CONFLICT(username) DO NOTHING`,
		id.String(), username, string(hash))
	if err != nil {
		return User{}, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return User{}, err
	} else if n == 0 {
		return User{}, ErrUsernameTaken
	}
	return User{ID: id.String(), Username: username}, nil
}

func (s *Store) Login(ctx context.Context, username, password string) (Tokens, error) {
	var id, hash string
	err := s.db.Read.QueryRowContext(ctx, `SELECT id, password_hash FROM users WHERE username = ?`, username).Scan(&id, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return Tokens{}, ErrBadCredentials
	}
	if err != nil {
		return Tokens{}, err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return Tokens{}, ErrBadCredentials
	}
	return s.issue(ctx, s.db.Write, id, time.Now())
}

// Refresh rotates a refresh token: the old one is deleted and a new pair is issued, atomically.
// shortcut: no reuse detection (replaying a rotated token doesn't revoke the user's other sessions); add a token-family table when hardening.
func (s *Store) Refresh(ctx context.Context, refreshToken string) (Tokens, error) {
	tx, err := s.db.Write.BeginTx(ctx, nil)
	if err != nil {
		return Tokens{}, err
	}
	defer tx.Rollback()

	now := time.Now()
	var userID string
	err = tx.QueryRowContext(ctx,
		`DELETE FROM refresh_tokens WHERE token_hash = ? AND expires_at > ? RETURNING user_id`,
		hashToken(refreshToken), now.UnixMilli()).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return Tokens{}, ErrBadRefresh
	}
	if err != nil {
		return Tokens{}, err
	}
	t, err := s.issue(ctx, tx, userID, now)
	if err != nil {
		return Tokens{}, err
	}
	return t, tx.Commit()
}

// Logout revokes one refresh token (this session). Unknown tokens are not an error, so the
// result never reveals whether a token was valid. The access JWT stays valid until it expires.
func (s *Store) Logout(ctx context.Context, refreshToken string) error {
	_, err := s.db.Write.ExecContext(ctx, `DELETE FROM refresh_tokens WHERE token_hash = ?`, hashToken(refreshToken))
	return err
}

// DeleteExpiredRefreshTokens removes refresh tokens that expired before now, in batches.
func (s *Store) DeleteExpiredRefreshTokens(ctx context.Context, now time.Time) (int64, error) {
	return db.DeleteInBatches(ctx, s.db.Write, deleteBatch, `DELETE FROM refresh_tokens WHERE token_hash IN
		(SELECT token_hash FROM refresh_tokens WHERE expires_at < ? LIMIT ?)`, now.UnixMilli())
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func (s *Store) issue(ctx context.Context, ex execer, userID string, now time.Time) (Tokens, error) {
	access, err := NewAccessToken(s.secret, userID, now)
	if err != nil {
		return Tokens{}, err
	}
	refresh := rand.Text() // 128+ bits of randomness, opaque (not a JWT) so it can only be checked against the DB
	// Store only the hash: a leaked DB copy then holds no usable refresh tokens.
	_, err = ex.ExecContext(ctx, `INSERT INTO refresh_tokens (token_hash, user_id, expires_at) VALUES (?, ?, ?)`,
		hashToken(refresh), userID, now.Add(RefreshTTL).UnixMilli())
	if err != nil {
		return Tokens{}, err
	}
	return Tokens{AccessToken: access, RefreshToken: refresh, ExpiresIn: int(AccessTTL.Seconds())}, nil
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// NewAccessToken signs a short-lived HS256 JWT whose subject is the user ID.
func NewAccessToken(secret []byte, userID string, now time.Time) (string, error) {
	return jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Subject:   userID,
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(AccessTTL)),
	}).SignedString(secret)
}

type ctxKey struct{}

// Require rejects requests without a valid access token (401) and puts the user ID in the request context.
func Require(secret []byte, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		var c jwt.RegisteredClaims
		if ok {
			// Pinning HS256 blocks "alg: none" and algorithm-confusion tokens.
			_, err := jwt.ParseWithClaims(raw, &c, func(*jwt.Token) (any, error) { return secret, nil },
				jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
			ok = err == nil && c.Subject != ""
		}
		if !ok {
			http.Error(w, "missing or invalid access token", http.StatusUnauthorized)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, c.Subject)))
	}
}

// UserID returns the authenticated user ID set by Require.
func UserID(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}
