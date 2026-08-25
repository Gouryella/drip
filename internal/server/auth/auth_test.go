package auth

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"drip/internal/server/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()

	s, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// seedClient creates an account plus a client and returns the plaintext token.
func seedClient(t *testing.T, s *store.Store) (string, *store.Client) {
	t.Helper()
	ctx := context.Background()

	acct, err := s.CreateAccount(ctx, "acme", 0)
	if err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	cred, err := GenerateCredential()
	if err != nil {
		t.Fatalf("GenerateCredential() error = %v", err)
	}

	client := &store.Client{
		ID:         cred.ID,
		AccountID:  acct.ID,
		Name:       "laptop",
		SecretHash: HashSecret(cred.Secret),
		Enabled:    true,
	}
	if err := s.CreateClient(ctx, client); err != nil {
		t.Fatalf("CreateClient() error = %v", err)
	}

	return cred.String(), client
}

// tamperToken returns token with its final character changed, so the secret is
// guaranteed to differ regardless of what the random secret ended with.
func tamperToken(token string) string {
	last := token[len(token)-1]
	replacement := byte('x')
	if last == replacement {
		replacement = 'y'
	}
	return token[:len(token)-1] + string(replacement)
}

func TestCredentialRoundTrip(t *testing.T) {
	cred, err := GenerateCredential()
	if err != nil {
		t.Fatalf("GenerateCredential() error = %v", err)
	}

	token := cred.String()
	if !IsCredential(token) {
		t.Fatalf("IsCredential(%q) = false, want true", token)
	}

	parsed, err := ParseCredential(token)
	if err != nil {
		t.Fatalf("ParseCredential() error = %v", err)
	}
	if parsed.ID != cred.ID || parsed.Secret != cred.Secret {
		t.Fatalf("ParseCredential() = %+v, want %+v", parsed, cred)
	}
}

// The secret half is base64url and may contain '_', which must not confuse the
// three-way split.
func TestParseCredentialWithUnderscoreInSecret(t *testing.T) {
	token := "drip_0123456789abcdef_abc_def_ghi"

	cred, err := ParseCredential(token)
	if err != nil {
		t.Fatalf("ParseCredential() error = %v", err)
	}
	if cred.ID != "0123456789abcdef" {
		t.Fatalf("ID = %q, want 0123456789abcdef", cred.ID)
	}
	if cred.Secret != "abc_def_ghi" {
		t.Fatalf("Secret = %q, want abc_def_ghi", cred.Secret)
	}
}

func TestParseCredentialRejectsMalformed(t *testing.T) {
	cases := []string{
		"",
		"plain-token",
		"drip_short_secret",
		"drip_zzzzzzzzzzzzzzzz_secret", // ID is not hex
		"drip_0123456789abcdef_",       // empty secret
		"other_0123456789abcdef_secret",
	}

	for _, token := range cases {
		if _, err := ParseCredential(token); err == nil {
			t.Errorf("ParseCredential(%q) = nil error, want failure", token)
		}
	}
}

func TestVerifySecret(t *testing.T) {
	hash := HashSecret("s3cret")

	if !VerifySecret("s3cret", hash) {
		t.Fatal("VerifySecret() = false for the correct secret")
	}
	if VerifySecret("wrong", hash) {
		t.Fatal("VerifySecret() = true for the wrong secret")
	}
}

func TestPasswordHashRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatalf("HashPassword() = %q, want a PHC argon2id string", hash)
	}

	ok, err := VerifyPassword("correct horse battery staple", hash)
	if err != nil {
		t.Fatalf("VerifyPassword() error = %v", err)
	}
	if !ok {
		t.Fatal("VerifyPassword() = false for the correct password")
	}

	ok, err = VerifyPassword("wrong", hash)
	if err != nil {
		t.Fatalf("VerifyPassword() error = %v", err)
	}
	if ok {
		t.Fatal("VerifyPassword() = true for the wrong password")
	}
}

func TestPasswordHashIsSalted(t *testing.T) {
	a, err := HashPassword("same")
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	b, err := HashPassword("same")
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	if a == b {
		t.Fatal("HashPassword() produced identical hashes for the same password")
	}
}

func TestAuthenticateCredential(t *testing.T) {
	s := newTestStore(t)
	token, client := seedClient(t, s)

	a := New(Config{Store: s})
	t.Cleanup(a.Close)

	identity, err := a.Authenticate(context.Background(), token, "203.0.113.7")
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if !identity.IsStored() {
		t.Fatal("Authenticate() returned an identity with no client")
	}
	if identity.ClientID() != client.ID {
		t.Fatalf("ClientID() = %q, want %q", identity.ClientID(), client.ID)
	}
	if identity.AccountID() != client.AccountID {
		t.Fatalf("AccountID() = %q, want %q", identity.AccountID(), client.AccountID)
	}
}

func TestAuthenticateRejectsWrongSecret(t *testing.T) {
	s := newTestStore(t)
	token, _ := seedClient(t, s)

	a := New(Config{Store: s})
	t.Cleanup(a.Close)

	tampered := tamperToken(token)
	if _, err := a.Authenticate(context.Background(), tampered, ""); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("Authenticate() error = %v, want ErrInvalidCredential", err)
	}
}

func TestAuthenticateRejectsUnknownCredential(t *testing.T) {
	s := newTestStore(t)
	a := New(Config{Store: s})
	t.Cleanup(a.Close)

	if _, err := a.Authenticate(context.Background(), "drip_0123456789abcdef_nope", ""); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("Authenticate() error = %v, want ErrInvalidCredential", err)
	}
}

func TestAuthenticateRejectsDisabledClient(t *testing.T) {
	s := newTestStore(t)
	token, client := seedClient(t, s)
	ctx := context.Background()

	client.Enabled = false
	if err := s.UpdateClient(ctx, client); err != nil {
		t.Fatalf("UpdateClient() error = %v", err)
	}

	a := New(Config{Store: s})
	t.Cleanup(a.Close)
	if _, err := a.Authenticate(ctx, token, ""); !errors.Is(err, ErrClientDisabled) {
		t.Fatalf("Authenticate() error = %v, want ErrClientDisabled", err)
	}
}

func TestAuthenticateRejectsDisabledAccount(t *testing.T) {
	s := newTestStore(t)
	token, client := seedClient(t, s)
	ctx := context.Background()

	acct, err := s.GetAccount(ctx, client.AccountID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	acct.Enabled = false
	if err := s.UpdateAccount(ctx, acct); err != nil {
		t.Fatalf("UpdateAccount() error = %v", err)
	}

	a := New(Config{Store: s})
	t.Cleanup(a.Close)
	if _, err := a.Authenticate(ctx, token, ""); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("Authenticate() error = %v, want ErrAccountDisabled", err)
	}
}

// Rotation must take effect immediately once the admin path invalidates the
// cache, not merely when the TTL lapses.
func TestInvalidateDropsCachedCredential(t *testing.T) {
	s := newTestStore(t)
	token, client := seedClient(t, s)
	ctx := context.Background()

	a := New(Config{Store: s})
	t.Cleanup(a.Close)
	if _, err := a.Authenticate(ctx, token, ""); err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}

	if err := s.RotateClientSecret(ctx, client.ID, HashSecret("brand-new")); err != nil {
		t.Fatalf("RotateClientSecret() error = %v", err)
	}
	a.Invalidate(client.ID)

	if _, err := a.Authenticate(ctx, token, ""); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("Authenticate() after rotation error = %v, want ErrInvalidCredential", err)
	}
}

// A token shaped like a credential must never fall through to the shared-token
// comparison, even when a legacy token is configured.
func TestCredentialNeverFallsThroughToLegacyToken(t *testing.T) {
	a := New(Config{LegacyToken: "drip_0123456789abcdef_shared"})
	t.Cleanup(a.Close)

	if _, err := a.Authenticate(context.Background(), "drip_0123456789abcdef_shared", ""); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("Authenticate() error = %v, want ErrInvalidCredential", err)
	}
}

func TestLegacyTokenAuthentication(t *testing.T) {
	a := New(Config{LegacyToken: "shared-secret"})
	t.Cleanup(a.Close)
	ctx := context.Background()

	identity, err := a.Authenticate(ctx, "shared-secret", "")
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if !identity.Legacy || identity.IsStored() {
		t.Fatalf("Authenticate() = %+v, want a legacy identity", identity)
	}

	if _, err := a.Authenticate(ctx, "wrong", ""); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("Authenticate() error = %v, want ErrInvalidCredential", err)
	}
}

func TestAnonymousModes(t *testing.T) {
	ctx := context.Background()

	open := New(Config{AllowAnonymous: true})
	t.Cleanup(open.Close)
	identity, err := open.Authenticate(ctx, "", "")
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if !identity.Anonymous {
		t.Fatalf("Authenticate() = %+v, want an anonymous identity", identity)
	}

	closed := New(Config{})
	t.Cleanup(closed.Close)
	if _, err := closed.Authenticate(ctx, "", ""); !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("Authenticate() error = %v, want ErrAuthRequired", err)
	}
}

func TestPurgeExpiredDoesNotDropLiveEntries(t *testing.T) {
	s := newTestStore(t)
	token, client := seedClient(t, s)

	a := New(Config{Store: s})
	t.Cleanup(a.Close)
	if _, err := a.Authenticate(context.Background(), token, ""); err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}

	a.PurgeExpired()

	a.mu.RLock()
	_, cached := a.cache[client.ID]
	a.mu.RUnlock()
	if !cached {
		t.Fatal("PurgeExpired() dropped an unexpired cache entry")
	}
}
