package tunnel

import (
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"
)

func newOwnerTestManager(t *testing.T) *Manager {
	t.Helper()

	m := NewManagerWithConfig(zap.NewNop(), ManagerConfig{
		MaxTunnels:      100,
		MaxTunnelsPerIP: 2,
		RateLimit:       3,
		RateLimitWindow: time.Minute,
	})
	t.Cleanup(m.Shutdown)
	return m
}

func TestRegisterOwnedRecordsOwner(t *testing.T) {
	m := newOwnerTestManager(t)
	owner := Owner{ClientID: "client-1", AccountID: "account-1"}

	subdomain, err := m.RegisterOwned(nil, "", "203.0.113.5", owner)
	if err != nil {
		t.Fatalf("RegisterOwned() error = %v", err)
	}

	conn, ok := m.Get(subdomain)
	if !ok {
		t.Fatalf("Get(%q) = not found", subdomain)
	}

	clientID, accountID := conn.Owner()
	if clientID != owner.ClientID || accountID != owner.AccountID {
		t.Fatalf("Owner() = (%q, %q), want (%q, %q)",
			clientID, accountID, owner.ClientID, owner.AccountID)
	}
}

// Authenticated clients frequently share one NAT egress IP, so the per-IP caps
// that protect anonymous servers must not apply to them.
func TestAuthenticatedClientsBypassPerIPLimits(t *testing.T) {
	m := newOwnerTestManager(t)
	owner := Owner{ClientID: "client-1", AccountID: "account-1"}

	const sharedIP = "203.0.113.9"
	for i := 0; i < 6; i++ {
		if _, err := m.RegisterOwned(nil, "", sharedIP, owner); err != nil {
			t.Fatalf("RegisterOwned() #%d error = %v", i+1, err)
		}
	}

	if got := m.Count(); got != 6 {
		t.Fatalf("Count() = %d, want 6", got)
	}
}

func TestAnonymousRegistrationsKeepPerIPLimit(t *testing.T) {
	m := newOwnerTestManager(t)

	const sharedIP = "203.0.113.11"
	for i := 0; i < 2; i++ {
		if _, err := m.RegisterWithIP(nil, "", sharedIP); err != nil {
			t.Fatalf("RegisterWithIP() #%d error = %v", i+1, err)
		}
	}

	if _, err := m.RegisterWithIP(nil, "", sharedIP); !errors.Is(err, ErrTooManyPerIP) {
		t.Fatalf("RegisterWithIP() error = %v, want ErrTooManyPerIP", err)
	}
}

func TestAccountTunnelLimit(t *testing.T) {
	m := newOwnerTestManager(t)
	owner := Owner{ClientID: "client-1", AccountID: "account-1", MaxTunnels: 2}

	first, err := m.RegisterOwned(nil, "", "203.0.113.20", owner)
	if err != nil {
		t.Fatalf("RegisterOwned() #1 error = %v", err)
	}
	if _, err := m.RegisterOwned(nil, "", "203.0.113.21", owner); err != nil {
		t.Fatalf("RegisterOwned() #2 error = %v", err)
	}

	if _, err := m.RegisterOwned(nil, "", "203.0.113.22", owner); !errors.Is(err, ErrTooManyForAccount) {
		t.Fatalf("RegisterOwned() #3 error = %v, want ErrTooManyForAccount", err)
	}

	// Freeing a tunnel must free the account slot with it.
	m.Unregister(first)
	if _, err := m.RegisterOwned(nil, "", "203.0.113.23", owner); err != nil {
		t.Fatalf("RegisterOwned() after unregister error = %v", err)
	}
}

// A rejected registration must not leave the account counter incremented.
func TestFailedRegistrationReleasesAccountSlot(t *testing.T) {
	m := newOwnerTestManager(t)
	owner := Owner{ClientID: "client-1", AccountID: "account-1", MaxTunnels: 2}

	taken, err := m.RegisterOwned(nil, "pinned", "203.0.113.30", owner)
	if err != nil {
		t.Fatalf("RegisterOwned() error = %v", err)
	}

	// Same subdomain, so registration fails after the account slot is reserved.
	if _, err := m.RegisterOwned(nil, taken, "203.0.113.31", owner); !errors.Is(err, ErrSubdomainTaken) {
		t.Fatalf("RegisterOwned() error = %v, want ErrSubdomainTaken", err)
	}

	// One slot is still free; if the rollback leaked, this would fail.
	if _, err := m.RegisterOwned(nil, "", "203.0.113.32", owner); err != nil {
		t.Fatalf("RegisterOwned() after rollback error = %v", err)
	}
}

func TestAccountLimitOfZeroIsUnlimited(t *testing.T) {
	m := newOwnerTestManager(t)
	owner := Owner{ClientID: "client-1", AccountID: "account-1", MaxTunnels: 0}

	for i := 0; i < 5; i++ {
		if _, err := m.RegisterOwned(nil, "", "203.0.113.40", owner); err != nil {
			t.Fatalf("RegisterOwned() #%d error = %v", i+1, err)
		}
	}
}
