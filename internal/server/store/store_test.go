package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"drip/internal/shared/utils"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()

	s, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// testClient builds a Client with a unique ID for insertion.
func testClient(accountID, name, hash string) *Client {
	return &Client{
		ID:         utils.GenerateShortID() + utils.GenerateShortID(),
		AccountID:  accountID,
		Name:       name,
		SecretHash: hash,
		Enabled:    true,
	}
}

func TestOpenAppliesMigrations(t *testing.T) {
	s := newTestStore(t)

	version, err := s.SchemaVersion(context.Background())
	if err != nil {
		t.Fatalf("SchemaVersion() error = %v", err)
	}
	if version != len(migrations) {
		t.Fatalf("SchemaVersion() = %d, want %d", version, len(migrations))
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")

	first, err := Open(path)
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	if _, err := first.CreateAccount(context.Background(), "acme", 0); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("second Open() error = %v", err)
	}
	defer func() { _ = second.Close() }()

	if _, err := second.GetAccountByName(context.Background(), "acme"); err != nil {
		t.Fatalf("GetAccountByName() after reopen error = %v", err)
	}
}

func TestAccountLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	acct, err := s.CreateAccount(ctx, "acme", 5)
	if err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}
	if !acct.Enabled || acct.MaxTunnels != 5 {
		t.Fatalf("CreateAccount() = %+v, want enabled with max 5", acct)
	}

	if _, err := s.CreateAccount(ctx, "acme", 0); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate CreateAccount() error = %v, want ErrConflict", err)
	}

	acct.Enabled = false
	acct.MaxTunnels = 9
	if err := s.UpdateAccount(ctx, acct); err != nil {
		t.Fatalf("UpdateAccount() error = %v", err)
	}

	got, err := s.GetAccount(ctx, acct.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if got.Enabled || got.MaxTunnels != 9 {
		t.Fatalf("GetAccount() = %+v, want disabled with max 9", got)
	}

	if err := s.DeleteAccount(ctx, acct.ID); err != nil {
		t.Fatalf("DeleteAccount() error = %v", err)
	}
	if _, err := s.GetAccount(ctx, acct.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetAccount() after delete error = %v, want ErrNotFound", err)
	}
}

func TestClientLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	acct, err := s.CreateAccount(ctx, "acme", 0)
	if err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	client := testClient(acct.ID, "laptop", "hash-a")
	if err := s.CreateClient(ctx, client); err != nil {
		t.Fatalf("CreateClient() error = %v", err)
	}

	dup := testClient(acct.ID, "laptop", "hash-b")
	dup.ID = "ffffffffffffffff"
	if err := s.CreateClient(ctx, dup); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate client name error = %v, want ErrConflict", err)
	}

	if err := s.RotateClientSecret(ctx, client.ID, "hash-c"); err != nil {
		t.Fatalf("RotateClientSecret() error = %v", err)
	}
	got, err := s.GetClient(ctx, client.ID)
	if err != nil {
		t.Fatalf("GetClient() error = %v", err)
	}
	if got.SecretHash != "hash-c" {
		t.Fatalf("SecretHash = %q, want hash-c", got.SecretHash)
	}

	if err := s.TouchClient(ctx, client.ID, "203.0.113.7"); err != nil {
		t.Fatalf("TouchClient() error = %v", err)
	}
	got, err = s.GetClient(ctx, client.ID)
	if err != nil {
		t.Fatalf("GetClient() after touch error = %v", err)
	}
	if got.LastSeenAt == nil || got.LastSeenIP != "203.0.113.7" {
		t.Fatalf("last seen = (%v, %q), want a timestamp and 203.0.113.7", got.LastSeenAt, got.LastSeenIP)
	}
}

func TestDeleteAccountCascadesToClients(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	acct, err := s.CreateAccount(ctx, "acme", 0)
	if err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}
	client := testClient(acct.ID, "laptop", "hash")
	if err := s.CreateClient(ctx, client); err != nil {
		t.Fatalf("CreateClient() error = %v", err)
	}

	if err := s.DeleteAccount(ctx, acct.ID); err != nil {
		t.Fatalf("DeleteAccount() error = %v", err)
	}

	if _, err := s.GetClient(ctx, client.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetClient() after account delete error = %v, want ErrNotFound", err)
	}
}

func TestGetClientNotFound(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.GetClient(context.Background(), "0123456789abcdef"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetClient() error = %v, want ErrNotFound", err)
	}
}
