package reservations

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"drip/internal/server/store"
)

type fixture struct {
	store   *store.Store
	account *store.Account
	client  *store.Client
	other   *store.Account
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	s, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()

	account, err := s.CreateAccount(ctx, "acme", 0)
	if err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}
	other, err := s.CreateAccount(ctx, "other", 0)
	if err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	client := &store.Client{
		ID:         "0123456789abcdef",
		AccountID:  account.ID,
		Name:       "laptop",
		SecretHash: "hash",
		Enabled:    true,
	}
	if err := s.CreateClient(ctx, client); err != nil {
		t.Fatalf("CreateClient() error = %v", err)
	}

	return &fixture{store: s, account: account, client: client, other: other}
}

func (f *fixture) reserve(t *testing.T, r *store.Reservation) *store.Reservation {
	t.Helper()

	if r.AccountID == "" {
		r.AccountID = f.account.ID
	}
	if err := f.store.CreateReservation(context.Background(), r); err != nil {
		t.Fatalf("CreateReservation() error = %v", err)
	}
	return r
}

func (f *fixture) request() Request {
	return Request{
		AccountID:  f.account.ID,
		ClientID:   f.client.ID,
		TunnelType: "http",
	}
}

func neverActive(string) bool { return false }

func alwaysActive(string) bool { return true }

// With no store, everything is ephemeral and the client's request passes through.
func TestResolverDisabledPassesThrough(t *testing.T) {
	r := New(nil, false, nil)

	got, err := r.Resolve(context.Background(), Request{
		TunnelType:         "http",
		RequestedSubdomain: "whatever",
	}, neverActive)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.Subdomain != "whatever" || got.IsReserved() {
		t.Fatalf("Resolve() = %+v, want an ephemeral passthrough", got)
	}
}

func TestAutoBindsOwnedReservation(t *testing.T) {
	f := newFixture(t)
	reservation := f.reserve(t, &store.Reservation{
		ClientID: &f.client.ID, TunnelType: store.TunnelTypeHTTP, Subdomain: "pinned", Enabled: true,
	})

	r := New(f.store, false, nil)

	got, err := r.Resolve(context.Background(), f.request(), neverActive)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.Subdomain != "pinned" {
		t.Fatalf("Subdomain = %q, want pinned", got.Subdomain)
	}
	if got.ReservationID != reservation.ID {
		t.Fatalf("ReservationID = %q, want %q", got.ReservationID, reservation.ID)
	}
}

// This is the whole point of the feature: the same client reconnecting must land
// on the same name every time.
func TestAutoBindIsStableAcrossReconnects(t *testing.T) {
	f := newFixture(t)
	f.reserve(t, &store.Reservation{
		ClientID: &f.client.ID, TunnelType: store.TunnelTypeHTTP, Subdomain: "stable", Enabled: true,
	})

	r := New(f.store, false, nil)

	for i := 0; i < 3; i++ {
		got, err := r.Resolve(context.Background(), f.request(), neverActive)
		if err != nil {
			t.Fatalf("Resolve() #%d error = %v", i+1, err)
		}
		if got.Subdomain != "stable" {
			t.Fatalf("Resolve() #%d subdomain = %q, want stable", i+1, got.Subdomain)
		}
	}
}

func TestAutoBindSkipsLiveReservations(t *testing.T) {
	f := newFixture(t)
	f.reserve(t, &store.Reservation{
		ClientID: &f.client.ID, TunnelType: store.TunnelTypeHTTP, Subdomain: "first", Enabled: true,
	})
	f.reserve(t, &store.Reservation{
		ClientID: &f.client.ID, TunnelType: store.TunnelTypeHTTP, Subdomain: "second", Enabled: true,
	})

	r := New(f.store, false, nil)

	got, err := r.Resolve(context.Background(), f.request(), func(subdomain string) bool {
		return subdomain == "first"
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.Subdomain != "second" {
		t.Fatalf("Subdomain = %q, want second", got.Subdomain)
	}
}

// Handing out a random name when the client's reservations are all busy would
// look like the reservation had been lost; say so instead.
func TestAutoBindReportsExhaustedReservations(t *testing.T) {
	f := newFixture(t)
	f.reserve(t, &store.Reservation{
		ClientID: &f.client.ID, TunnelType: store.TunnelTypeHTTP, Subdomain: "busy", Enabled: true,
	})

	r := New(f.store, false, nil)

	if _, err := r.Resolve(context.Background(), f.request(), alwaysActive); !errors.Is(err, ErrReservationInUse) {
		t.Fatalf("Resolve() error = %v, want ErrReservationInUse", err)
	}
}

func TestClientWithoutReservationsGetsEphemeral(t *testing.T) {
	f := newFixture(t)
	r := New(f.store, false, nil)

	got, err := r.Resolve(context.Background(), f.request(), neverActive)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.Subdomain != "" || got.IsReserved() {
		t.Fatalf("Resolve() = %+v, want an ephemeral tunnel", got)
	}
}

func TestExplicitRequestForOwnReservation(t *testing.T) {
	f := newFixture(t)
	f.reserve(t, &store.Reservation{
		ClientID: &f.client.ID, TunnelType: store.TunnelTypeHTTP, Subdomain: "mine", Enabled: true,
	})

	r := New(f.store, false, nil)
	req := f.request()
	req.RequestedSubdomain = "mine"

	got, err := r.Resolve(context.Background(), req, neverActive)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if !got.IsReserved() || got.Subdomain != "mine" {
		t.Fatalf("Resolve() = %+v, want the reservation bound", got)
	}
}

func TestExplicitRequestForAnotherAccountsReservation(t *testing.T) {
	f := newFixture(t)
	f.reserve(t, &store.Reservation{
		AccountID: f.other.ID, TunnelType: store.TunnelTypeHTTP, Subdomain: "theirs", Enabled: true,
	})

	r := New(f.store, false, nil)
	req := f.request()
	req.RequestedSubdomain = "theirs"

	if _, err := r.Resolve(context.Background(), req, neverActive); !errors.Is(err, ErrReservedByAnother) {
		t.Fatalf("Resolve() error = %v, want ErrReservedByAnother", err)
	}
}

// An unauthenticated registration owns nothing, so it must not be able to grab
// a reserved name by simply asking for it.
func TestAnonymousCannotTakeAReservedName(t *testing.T) {
	f := newFixture(t)
	f.reserve(t, &store.Reservation{
		TunnelType: store.TunnelTypeHTTP, Subdomain: "owned", Enabled: true,
	})

	r := New(f.store, false, nil)

	_, err := r.Resolve(context.Background(), Request{
		TunnelType:         "http",
		RequestedSubdomain: "owned",
	}, neverActive)
	if !errors.Is(err, ErrReservedByAnother) {
		t.Fatalf("Resolve() error = %v, want ErrReservedByAnother", err)
	}
}

func TestReservationBoundToAnotherClient(t *testing.T) {
	f := newFixture(t)

	otherClient := "fedcba9876543210"
	if err := f.store.CreateClient(context.Background(), &store.Client{
		ID: otherClient, AccountID: f.account.ID, Name: "desktop", SecretHash: "hash", Enabled: true,
	}); err != nil {
		t.Fatalf("CreateClient() error = %v", err)
	}
	f.reserve(t, &store.Reservation{
		ClientID: &otherClient, TunnelType: store.TunnelTypeHTTP, Subdomain: "desktoponly", Enabled: true,
	})

	r := New(f.store, false, nil)
	req := f.request()
	req.RequestedSubdomain = "desktoponly"

	if _, err := r.Resolve(context.Background(), req, neverActive); !errors.Is(err, ErrNotBoundToClient) {
		t.Fatalf("Resolve() error = %v, want ErrNotBoundToClient", err)
	}
}

// An unbound reservation belongs to the account, so any of its clients may claim
// it by asking for the name.
func TestUnboundReservationIsClaimableByAccount(t *testing.T) {
	f := newFixture(t)
	f.reserve(t, &store.Reservation{
		TunnelType: store.TunnelTypeHTTP, Subdomain: "shared", Enabled: true,
	})

	r := New(f.store, false, nil)
	req := f.request()
	req.RequestedSubdomain = "shared"

	got, err := r.Resolve(context.Background(), req, neverActive)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if !got.IsReserved() {
		t.Fatalf("Resolve() = %+v, want the reservation bound", got)
	}
}

func TestDisabledReservationIsRefused(t *testing.T) {
	f := newFixture(t)
	f.reserve(t, &store.Reservation{
		ClientID: &f.client.ID, TunnelType: store.TunnelTypeHTTP, Subdomain: "off",
	})

	r := New(f.store, false, nil)
	req := f.request()
	req.RequestedSubdomain = "off"

	if _, err := r.Resolve(context.Background(), req, neverActive); !errors.Is(err, ErrReservationDisabled) {
		t.Fatalf("Resolve() error = %v, want ErrReservationDisabled", err)
	}
}

func TestReservationsOnlyMode(t *testing.T) {
	f := newFixture(t)
	r := New(f.store, true, nil)

	// No reservation at all.
	if _, err := r.Resolve(context.Background(), f.request(), neverActive); !errors.Is(err, ErrReservationRequired) {
		t.Fatalf("Resolve() error = %v, want ErrReservationRequired", err)
	}

	// An unreserved name is not a way around it either.
	req := f.request()
	req.RequestedSubdomain = "freename"
	if _, err := r.Resolve(context.Background(), req, neverActive); !errors.Is(err, ErrReservationRequired) {
		t.Fatalf("Resolve() with a free name error = %v, want ErrReservationRequired", err)
	}

	// With a reservation it goes through.
	f.reserve(t, &store.Reservation{
		ClientID: &f.client.ID, TunnelType: store.TunnelTypeHTTP, Subdomain: "allowed", Enabled: true,
	})
	got, err := r.Resolve(context.Background(), f.request(), neverActive)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.Subdomain != "allowed" {
		t.Fatalf("Subdomain = %q, want allowed", got.Subdomain)
	}
}

func TestTCPPortReservation(t *testing.T) {
	f := newFixture(t)
	f.reserve(t, &store.Reservation{
		ClientID: &f.client.ID, TunnelType: store.TunnelTypeTCP, TCPPort: 30007, Enabled: true,
	})

	r := New(f.store, false, nil)
	req := f.request()
	req.TunnelType = "tcp"

	got, err := r.Resolve(context.Background(), req, neverActive)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.TCPPort != 30007 {
		t.Fatalf("TCPPort = %d, want 30007", got.TCPPort)
	}
	// The manager keys TCP tunnels by a derived subdomain.
	if got.Subdomain != "tcp-30007" {
		t.Fatalf("Subdomain = %q, want tcp-30007", got.Subdomain)
	}
}

func TestTCPPortReservedByAnotherAccount(t *testing.T) {
	f := newFixture(t)
	f.reserve(t, &store.Reservation{
		AccountID: f.other.ID, TunnelType: store.TunnelTypeTCP, TCPPort: 30008, Enabled: true,
	})

	r := New(f.store, false, nil)
	req := f.request()
	req.TunnelType = "tcp"
	req.RequestedTCPPort = 30008

	if _, err := r.Resolve(context.Background(), req, neverActive); !errors.Is(err, ErrPortReservedByAnother) {
		t.Fatalf("Resolve() error = %v, want ErrPortReservedByAnother", err)
	}
}

// An https tunnel must bind a reservation recorded as http: the name is the same.
func TestHTTPSBindsHTTPReservation(t *testing.T) {
	f := newFixture(t)
	f.reserve(t, &store.Reservation{
		ClientID: &f.client.ID, TunnelType: store.TunnelTypeHTTP, Subdomain: "secure", Enabled: true,
	})

	r := New(f.store, false, nil)
	req := f.request()
	req.TunnelType = "https"

	got, err := r.Resolve(context.Background(), req, neverActive)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.Subdomain != "secure" {
		t.Fatalf("Subdomain = %q, want secure", got.Subdomain)
	}
}
