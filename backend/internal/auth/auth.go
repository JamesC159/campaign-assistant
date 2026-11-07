// Package auth handles accounts: email+password signup and login, passwordless
// magic links, and server-side sessions identified by an opaque cookie token.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	netmail "net/mail"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"campaign-assistant/backend/internal/mail"
	"campaign-assistant/backend/internal/store"
	"campaign-assistant/backend/internal/store/dbq"
)

// Errors returned to handlers. They are deliberately coarse so responses don't
// reveal whether an email has an account (except at signup, where it must).
var (
	ErrInvalidEmail       = errors.New("enter a valid email address")
	ErrWeakPassword       = errors.New("password must be 10 to 256 characters")
	ErrEmailTaken         = errors.New("an account with this email already exists")
	ErrInvalidCredentials = errors.New("email or password is incorrect")
	ErrInvalidLink        = errors.New("this sign-in link is invalid or has expired")
	ErrNoSession          = errors.New("not signed in")
)

// User is a signed-in account.
type User struct {
	ID    uuid.UUID `json:"id"`
	Email string    `json:"email"`
}

// Service implements the account flows.
type Service struct {
	Store      *store.Store
	Mail       mail.Sender
	Log        *slog.Logger
	Params     Params
	AppURL     string        // public URL of the web app, for links in email
	SessionTTL time.Duration // default 30 days
	MagicTTL   time.Duration // default 15 minutes

	dummyOnce sync.Once
	dummyHash string // compared against for unknown emails, so login timing doesn't reveal accounts
}

func (s *Service) sessionTTL() time.Duration {
	if s.SessionTTL > 0 {
		return s.SessionTTL
	}
	return 30 * 24 * time.Hour
}

func (s *Service) magicTTL() time.Duration {
	if s.MagicTTL > 0 {
		return s.MagicTTL
	}
	return 15 * time.Minute
}

// Signup creates an account and signs it in, returning the session token.
func (s *Service) Signup(ctx context.Context, email, password string) (User, string, error) {
	email, err := normalizeEmail(email)
	if err != nil {
		return User{}, "", err
	}
	if n := utf8.RuneCountInString(password); n < 10 || n > 256 {
		return User{}, "", ErrWeakPassword
	}
	hash, err := HashPassword(password, s.Params)
	if err != nil {
		return User{}, "", err
	}
	var u User
	err = s.Store.System(ctx, func(q *dbq.Queries) error {
		row, err := q.CreateUser(ctx, dbq.CreateUserParams{Email: email, PasswordHash: &hash})
		if err != nil {
			return err
		}
		u = User{ID: row.ID, Email: row.Email}
		return nil
	})
	if isUniqueViolation(err) {
		return User{}, "", ErrEmailTaken
	}
	if err != nil {
		return User{}, "", fmt.Errorf("create user: %w", err)
	}
	token, err := s.startSession(ctx, u.ID)
	return u, token, err
}

// Login checks a password and starts a session.
func (s *Service) Login(ctx context.Context, email, password string) (User, string, error) {
	email, err := normalizeEmail(email)
	if err != nil {
		return User{}, "", ErrInvalidCredentials
	}
	row, err := s.Store.Q().GetUserByEmail(ctx, email)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return User{}, "", err
	}
	found := err == nil && row.PasswordHash != nil // magic-link-only accounts have no password
	hash := s.fakeHash()
	if found {
		hash = *row.PasswordHash
	}
	ok, _ := VerifyPassword(password, hash) // always hash, so timing doesn't reveal accounts
	if !found || !ok {
		return User{}, "", ErrInvalidCredentials
	}
	token, err := s.startSession(ctx, row.ID)
	return User{ID: row.ID, Email: row.Email}, token, err
}

// Authenticate resolves a session token to its user.
func (s *Service) Authenticate(ctx context.Context, token string) (User, error) {
	if token == "" {
		return User{}, ErrNoSession
	}
	row, err := s.Store.Q().GetAuthSession(ctx, hashToken(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNoSession
	}
	if err != nil {
		return User{}, err
	}
	if time.Since(row.LastSeenAt) > time.Hour { // keep last_seen useful without a write per request
		_ = s.Store.Q().TouchAuthSession(ctx, row.ID)
	}
	return User{ID: row.UserID, Email: row.Email}, nil
}

// Logout ends a session. Unknown tokens are ignored.
func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return s.Store.Q().DeleteAuthSession(ctx, hashToken(token))
}

// SendMagicLink emails a one-time sign-in link, creating a passwordless
// account if the email is new. It never reveals whether the account existed.
func (s *Service) SendMagicLink(ctx context.Context, email string) error {
	email, err := normalizeEmail(email)
	if err != nil {
		return err
	}
	token, hash, err := newToken()
	if err != nil {
		return err
	}
	err = s.Store.System(ctx, func(q *dbq.Queries) error {
		var userID uuid.UUID
		row, err := q.GetUserByEmail(ctx, email)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			created, err := q.CreateUser(ctx, dbq.CreateUserParams{Email: email})
			if err != nil {
				return err
			}
			userID = created.ID
		case err != nil:
			return err
		default:
			userID = row.ID
		}
		return q.CreateMagicLink(ctx, dbq.CreateMagicLinkParams{UserID: userID, TokenHash: hash, ExpiresAt: time.Now().Add(s.magicTTL())})
	})
	if err != nil {
		return fmt.Errorf("create magic link: %w", err)
	}

	link := strings.TrimRight(s.AppURL, "/") + "/auth/verify?token=" + url.QueryEscape(token)
	return s.Mail.Send(ctx, mail.Message{
		To:      email,
		Subject: "Your sign-in link",
		Body: fmt.Sprintf("Sign in to Campaign Assistant:\n\n%s\n\nThis link works once and expires in %d minutes. "+
			"If you didn't ask for it, you can ignore this email.\n", link, int(s.magicTTL().Minutes())),
	})
}

// VerifyMagicLink consumes a link token and starts a session.
func (s *Service) VerifyMagicLink(ctx context.Context, token string) (User, string, error) {
	if token == "" {
		return User{}, "", ErrInvalidLink
	}
	var u User
	err := s.Store.System(ctx, func(q *dbq.Queries) error {
		userID, err := q.UseMagicLink(ctx, hashToken(token))
		if err != nil {
			return err
		}
		row, err := q.GetUserByID(ctx, userID)
		u = User{ID: row.ID, Email: row.Email}
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, "", ErrInvalidLink
	}
	if err != nil {
		return User{}, "", err
	}
	session, err := s.startSession(ctx, u.ID)
	return u, session, err
}

func (s *Service) startSession(ctx context.Context, userID uuid.UUID) (string, error) {
	token, hash, err := newToken()
	if err != nil {
		return "", err
	}
	if err := s.Store.Q().CreateAuthSession(ctx, dbq.CreateAuthSessionParams{UserID: userID, TokenHash: hash, ExpiresAt: time.Now().Add(s.sessionTTL())}); err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	return token, nil
}

// SessionTTLValue exposes the session lifetime for cookie Max-Age.
func (s *Service) SessionTTLValue() time.Duration { return s.sessionTTL() }

func (s *Service) fakeHash() string {
	s.dummyOnce.Do(func() { s.dummyHash, _ = HashPassword("timing-equalizer", s.Params) })
	return s.dummyHash
}

func normalizeEmail(email string) (string, error) {
	email = strings.TrimSpace(email)
	addr, err := netmail.ParseAddress(email)
	if err != nil || addr.Address != email || len(email) > 254 || !strings.Contains(email, ".") {
		return "", ErrInvalidEmail
	}
	return strings.ToLower(email), nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
