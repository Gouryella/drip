package store

import (
	"context"
	"errors"
	"testing"
)

func seedAccount(t *testing.T, s *Store, name string) *Account {
	t.Helper()

	acct, err := s.CreateAccount(context.Background(), name, 0)
	if err != nil {
		t.Fatalf("CreateAccount(%q) error = %v", name, err)
	}
	return acct
}

func TestNormalizeTunnelType(t *testing.T) {
	cases := map[string]string{
		"http":  TunnelTypeHTTP,
		"https": TunnelTypeHTTP,
		"HTTPS": TunnelTypeHTTP,
		" tcp ": TunnelTypeTCP,
	}

	for in, want := range cases {
		if got := NormalizeTunnelType(in); got != want {
			t.Errorf("NormalizeTunnelType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCreateSubdomainReservation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	acct := seedAccount(t, s, "acme")

	r := &Reservation{AccountID: acct.ID, TunnelType: TunnelTypeHTTPS, Subdomain: "MyApp", Enabled: true}
	if err := s.CreateReservation(ctx, r); err != nil {
		t.Fatalf("CreateReservation() error = %v", err)
	}

	// https collapses into the http family, and the name is lowercased.
	if r.TunnelType != TunnelTypeHTTP {
		t.Fatalf("TunnelType = %q, want %q", r.TunnelType, TunnelTypeHTTP)
	}
	if r.Subdomain != "myapp" {
		t.Fatalf("Subdomain = %q, want myapp", r.Subdomain)
	}

	got, err := s.GetReservationBySubdomain(ctx, "MYAPP")
	if err != nil {
		t.Fatalf("GetReservationBySubdomain() error = %v", err)
	}
	if got.ID != r.ID {
		t.Fatalf("GetReservationBySubdomain() = %q, want %q", got.ID, r.ID)
	}
}

func TestReservationValidation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	acct := seedAccount(t, s, "acme")

	cases := []struct {
		name string
		r    *Reservation
	}{
		{"http without a subdomain", &Reservation{AccountID: acct.ID, TunnelType: TunnelTypeHTTP}},
		{"http with a port", &Reservation{AccountID: acct.ID, TunnelType: TunnelTypeHTTP, Subdomain: "myservice", TCPPort: 30000}},
		{"tcp without a port", &Reservation{AccountID: acct.ID, TunnelType: TunnelTypeTCP}},
		{"tcp with a subdomain", &Reservation{AccountID: acct.ID, TunnelType: TunnelTypeTCP, TCPPort: 30000, Subdomain: "myservice"}},
		{"invalid subdomain", &Reservation{AccountID: acct.ID, TunnelType: TunnelTypeHTTP, Subdomain: "a"}},
		{"server-reserved subdomain", &Reservation{AccountID: acct.ID, TunnelType: TunnelTypeHTTP, Subdomain: "admin"}},
		{"unknown tunnel type", &Reservation{AccountID: acct.ID, TunnelType: "udp", Subdomain: "app"}},
		{"no account", &Reservation{TunnelType: TunnelTypeHTTP, Subdomain: "myservice"}},
		{"port out of range", &Reservation{AccountID: acct.ID, TunnelType: TunnelTypeTCP, TCPPort: 70000}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.CreateReservation(ctx, tc.r); err == nil {
				t.Fatal("CreateReservation() = nil, want error")
			}
		})
	}
}

// A subdomain and a port may each be reserved once across the whole server.
func TestReservationUniqueness(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	first := seedAccount(t, s, "acme")
	second := seedAccount(t, s, "other")

	if err := s.CreateReservation(ctx, &Reservation{
		AccountID: first.ID, TunnelType: TunnelTypeHTTP, Subdomain: "shared", Enabled: true,
	}); err != nil {
		t.Fatalf("CreateReservation() error = %v", err)
	}

	err := s.CreateReservation(ctx, &Reservation{
		AccountID: second.ID, TunnelType: TunnelTypeHTTP, Subdomain: "shared", Enabled: true,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate subdomain error = %v, want ErrConflict", err)
	}

	if err := s.CreateReservation(ctx, &Reservation{
		AccountID: first.ID, TunnelType: TunnelTypeTCP, TCPPort: 30001, Enabled: true,
	}); err != nil {
		t.Fatalf("CreateReservation() error = %v", err)
	}
	err = s.CreateReservation(ctx, &Reservation{
		AccountID: second.ID, TunnelType: TunnelTypeTCP, TCPPort: 30001, Enabled: true,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate port error = %v, want ErrConflict", err)
	}
}

// The partial unique indexes must not treat the unused column as a collision:
// many http reservations all carry tcp_port 0.
func TestManyReservationsShareUnusedColumns(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	acct := seedAccount(t, s, "acme")

	for _, name := range []string{"one", "two", "three"} {
		if err := s.CreateReservation(ctx, &Reservation{
			AccountID: acct.ID, TunnelType: TunnelTypeHTTP, Subdomain: name, Enabled: true,
		}); err != nil {
			t.Fatalf("CreateReservation(%q) error = %v", name, err)
		}
	}

	for _, port := range []int{30001, 30002, 30003} {
		if err := s.CreateReservation(ctx, &Reservation{
			AccountID: acct.ID, TunnelType: TunnelTypeTCP, TCPPort: port, Enabled: true,
		}); err != nil {
			t.Fatalf("CreateReservation(port %d) error = %v", port, err)
		}
	}

	list, err := s.ListReservations(ctx, acct.ID)
	if err != nil {
		t.Fatalf("ListReservations() error = %v", err)
	}
	if len(list) != 6 {
		t.Fatalf("ListReservations() = %d rows, want 6", len(list))
	}
}

func TestListReservationsForClient(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	acct := seedAccount(t, s, "acme")

	client := testClient(acct.ID, "laptop", "hash")
	if err := s.CreateClient(ctx, client); err != nil {
		t.Fatalf("CreateClient() error = %v", err)
	}

	bound := &Reservation{AccountID: acct.ID, ClientID: &client.ID, TunnelType: TunnelTypeHTTP, Subdomain: "bound", Enabled: true}
	unbound := &Reservation{AccountID: acct.ID, TunnelType: TunnelTypeHTTP, Subdomain: "unbound", Enabled: true}
	disabled := &Reservation{AccountID: acct.ID, ClientID: &client.ID, TunnelType: TunnelTypeHTTP, Subdomain: "disabled"}
	tcpRes := &Reservation{AccountID: acct.ID, ClientID: &client.ID, TunnelType: TunnelTypeTCP, TCPPort: 30005, Enabled: true}

	for _, r := range []*Reservation{bound, unbound, disabled, tcpRes} {
		if err := s.CreateReservation(ctx, r); err != nil {
			t.Fatalf("CreateReservation(%s) error = %v", r.Target(), err)
		}
	}

	httpList, err := s.ListReservationsForClient(ctx, client.ID, "https")
	if err != nil {
		t.Fatalf("ListReservationsForClient() error = %v", err)
	}
	if len(httpList) != 1 || httpList[0].Subdomain != "bound" {
		t.Fatalf("ListReservationsForClient(http) = %+v, want only the enabled bound reservation", httpList)
	}

	tcpList, err := s.ListReservationsForClient(ctx, client.ID, TunnelTypeTCP)
	if err != nil {
		t.Fatalf("ListReservationsForClient(tcp) error = %v", err)
	}
	if len(tcpList) != 1 || tcpList[0].TCPPort != 30005 {
		t.Fatalf("ListReservationsForClient(tcp) = %+v, want the port reservation", tcpList)
	}
}

// Deleting a client must release its reservations back to the account rather
// than deleting them: the name is the account's, the binding is not.
func TestDeletingClientUnbindsReservation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	acct := seedAccount(t, s, "acme")

	client := testClient(acct.ID, "laptop", "hash")
	if err := s.CreateClient(ctx, client); err != nil {
		t.Fatalf("CreateClient() error = %v", err)
	}

	r := &Reservation{AccountID: acct.ID, ClientID: &client.ID, TunnelType: TunnelTypeHTTP, Subdomain: "keepme", Enabled: true}
	if err := s.CreateReservation(ctx, r); err != nil {
		t.Fatalf("CreateReservation() error = %v", err)
	}

	if err := s.DeleteClient(ctx, client.ID); err != nil {
		t.Fatalf("DeleteClient() error = %v", err)
	}

	got, err := s.GetReservation(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetReservation() after client delete error = %v", err)
	}
	if got.ClientID != nil {
		t.Fatalf("ClientID = %v, want nil after the client was deleted", *got.ClientID)
	}
}

func TestUpdateAndDeleteReservation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	acct := seedAccount(t, s, "acme")

	r := &Reservation{AccountID: acct.ID, TunnelType: TunnelTypeHTTP, Subdomain: "myservice", Enabled: true}
	if err := s.CreateReservation(ctx, r); err != nil {
		t.Fatalf("CreateReservation() error = %v", err)
	}

	r.Enabled = false
	r.Bandwidth = "2M"
	if err := s.UpdateReservation(ctx, r); err != nil {
		t.Fatalf("UpdateReservation() error = %v", err)
	}

	got, err := s.GetReservation(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetReservation() error = %v", err)
	}
	if got.Enabled || got.Bandwidth != "2M" {
		t.Fatalf("GetReservation() = %+v, want disabled with 2M", got)
	}

	if err := s.DeleteReservation(ctx, r.ID); err != nil {
		t.Fatalf("DeleteReservation() error = %v", err)
	}

	// The name is free again once the reservation is gone.
	if err := s.CreateReservation(ctx, &Reservation{
		AccountID: acct.ID, TunnelType: TunnelTypeHTTP, Subdomain: "myservice", Enabled: true,
	}); err != nil {
		t.Fatalf("CreateReservation() after delete error = %v", err)
	}
}
