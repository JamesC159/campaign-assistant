package auth_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"campaign-assistant/backend/internal/auth"
	"campaign-assistant/backend/internal/mail"
	"campaign-assistant/backend/internal/store"
	"campaign-assistant/backend/internal/testdb"
)

func TestPasswordHashRoundTrip(t *testing.T) {
	h, err := auth.HashPassword("correct horse battery", auth.TestParams)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=1024,t=1,p=1$") {
		t.Errorf("unexpected hash format %q", h)
	}
	if ok, err := auth.VerifyPassword("correct horse battery", h); !ok || err != nil {
		t.Errorf("right password rejected: %v", err)
	}
	if ok, _ := auth.VerifyPassword("wrong horse battery", h); ok {
		t.Error("wrong password accepted")
	}
	h2, _ := auth.HashPassword("correct horse battery", auth.TestParams)
	if h == h2 {
		t.Error("hashes must be salted")
	}
	if _, err := auth.VerifyPassword("x", "$bcrypt$nope"); err == nil {
		t.Error("malformed hash must error")
	}
}

func newService(t *testing.T) (*auth.Service, *mail.Mailbox) {
	d := testdb.New(t)
	box := &mail.Mailbox{}
	return &auth.Service{Store: store.New(d.App), Mail: box, Params: auth.TestParams, AppURL: "http://app.test"}, box
}

func TestSignupLoginLogout(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()

	u, token, err := s.Signup(ctx, "GM@Example.com", "a long enough password")
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "gm@example.com" || token == "" {
		t.Fatalf("signup returned %+v, %q", u, token)
	}
	if _, _, err := s.Signup(ctx, "gm@example.com", "another long password"); !errors.Is(err, auth.ErrEmailTaken) {
		t.Errorf("duplicate email (case-insensitive) must be rejected, got %v", err)
	}
	if _, _, err := s.Signup(ctx, "x@y.test", "short"); !errors.Is(err, auth.ErrWeakPassword) {
		t.Errorf("short password must be rejected, got %v", err)
	}
	if _, _, err := s.Signup(ctx, "not-an-email", "a long enough password"); !errors.Is(err, auth.ErrInvalidEmail) {
		t.Errorf("bad email must be rejected, got %v", err)
	}

	if got, err := s.Authenticate(ctx, token); err != nil || got.ID != u.ID {
		t.Fatalf("signup session should authenticate: %+v %v", got, err)
	}
	if _, _, err := s.Login(ctx, "gm@example.com", "wrong password!!"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("wrong password: %v", err)
	}
	if _, _, err := s.Login(ctx, "nobody@example.com", "a long enough password"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("unknown user must look like a wrong password: %v", err)
	}
	_, token2, err := s.Login(ctx, "gm@example.com", "a long enough password")
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Logout(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, token); !errors.Is(err, auth.ErrNoSession) {
		t.Errorf("logged-out token must not authenticate: %v", err)
	}
	if _, err := s.Authenticate(ctx, token2); err != nil {
		t.Errorf("logout must only end its own session: %v", err)
	}
}

func TestMagicLinkIsSingleUse(t *testing.T) {
	s, box := newService(t)
	ctx := context.Background()

	if err := s.SendMagicLink(ctx, "player@example.com"); err != nil {
		t.Fatal(err)
	}
	msgs := box.Messages()
	if len(msgs) != 1 || msgs[0].To != "player@example.com" {
		t.Fatalf("expected one email, got %+v", msgs)
	}
	token := linkToken(t, msgs[0].Body)

	u, session, err := s.VerifyMagicLink(ctx, token)
	if err != nil || u.Email != "player@example.com" || session == "" {
		t.Fatalf("verify: %+v %q %v", u, session, err)
	}
	if _, _, err := s.VerifyMagicLink(ctx, token); !errors.Is(err, auth.ErrInvalidLink) {
		t.Errorf("second use must fail, got %v", err)
	}
	if _, _, err := s.VerifyMagicLink(ctx, "forged"); !errors.Is(err, auth.ErrInvalidLink) {
		t.Errorf("forged token must fail, got %v", err)
	}
	// A magic-link-only account has no password, so password login must fail.
	if _, _, err := s.Login(ctx, "player@example.com", "anything at all"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("passwordless account must not log in with a password: %v", err)
	}
}

func linkToken(t *testing.T, body string) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "http") {
			u, err := url.Parse(line)
			if err != nil {
				t.Fatal(err)
			}
			return u.Query().Get("token")
		}
	}
	t.Fatalf("no link in %q", body)
	return ""
}
