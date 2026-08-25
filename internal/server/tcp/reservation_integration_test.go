package tcp

import (
	"testing"

	json "github.com/goccy/go-json"

	"drip/internal/server/store"
	"drip/internal/shared/protocol"
)

// registerOK performs a registration and requires it to succeed.
func registerOK(t *testing.T, ts *authTestServer, req protocol.RegisterRequest) protocol.RegisterResponse {
	t.Helper()

	frame := ts.register(t, req)
	if frame.Type != protocol.FrameTypeRegisterAck {
		t.Fatalf("frame type = %s, want RegisterAck (error: %+v)", frame.Type, decodeError(t, frame))
	}

	var resp protocol.RegisterResponse
	if err := json.Unmarshal(frame.Payload, &resp); err != nil {
		t.Fatalf("unmarshal ack: %v", err)
	}
	return resp
}

// A client with a reservation and no requested subdomain lands on its reserved
// name - the behavior the whole feature exists for.
func TestReservedClientBindsItsSubdomain(t *testing.T) {
	ts := newTestServer(t, true, false)
	token, acct, client := ts.seedAccountCredential(t, "acme", 0)

	if err := ts.store.CreateReservation(t.Context(), &store.Reservation{
		AccountID:  acct.ID,
		ClientID:   &client.ID,
		TunnelType: store.TunnelTypeHTTP,
		Subdomain:  "reserved-app",
		Enabled:    true,
	}); err != nil {
		t.Fatalf("CreateReservation() error = %v", err)
	}

	resp := registerOK(t, ts, protocol.RegisterRequest{
		Token:      token,
		TunnelType: protocol.TunnelTypeHTTP,
		LocalPort:  8080,
	})

	if resp.Subdomain != "reserved-app" {
		t.Fatalf("Subdomain = %q, want reserved-app", resp.Subdomain)
	}
}

// A reserved name must not be grabbable by a different account.
func TestReservedSubdomainRefusedToOtherAccount(t *testing.T) {
	ts := newTestServer(t, true, false)

	_, owner, ownerClient := ts.seedAccountCredential(t, "acme", 0)
	if err := ts.store.CreateReservation(t.Context(), &store.Reservation{
		AccountID:  owner.ID,
		ClientID:   &ownerClient.ID,
		TunnelType: store.TunnelTypeHTTP,
		Subdomain:  "contested",
		Enabled:    true,
	}); err != nil {
		t.Fatalf("CreateReservation() error = %v", err)
	}

	intruderToken, _, _ := ts.seedAccountCredential(t, "intruder", 0)

	frame := ts.register(t, protocol.RegisterRequest{
		Token:           intruderToken,
		TunnelType:      protocol.TunnelTypeHTTP,
		CustomSubdomain: "contested",
		LocalPort:       8080,
	})

	if frame.Type != protocol.FrameTypeError {
		t.Fatalf("frame type = %s, want Error", frame.Type)
	}
}

// Without a reservation the client still gets a random name, so ordinary
// self-hosted use is unaffected by the feature.
func TestUnreservedClientStillGetsEphemeralTunnel(t *testing.T) {
	ts := newTestServer(t, true, false)
	token := ts.seedCredential(t, 0)

	resp := registerOK(t, ts, protocol.RegisterRequest{
		Token:      token,
		TunnelType: protocol.TunnelTypeHTTP,
		LocalPort:  8080,
	})

	if resp.Subdomain == "" {
		t.Fatal("Subdomain is empty, want a generated name")
	}
}

func TestReservationsOnlyRejectsUnreservedClient(t *testing.T) {
	ts := newTestServer(t, true, true)
	token := ts.seedCredential(t, 0)

	frame := ts.register(t, protocol.RegisterRequest{
		Token:      token,
		TunnelType: protocol.TunnelTypeHTTP,
		LocalPort:  8080,
	})

	if frame.Type != protocol.FrameTypeError {
		t.Fatalf("frame type = %s, want Error", frame.Type)
	}
}

func TestReservationsOnlyAcceptsReservedClient(t *testing.T) {
	ts := newTestServer(t, true, true)
	token, acct, client := ts.seedAccountCredential(t, "acme", 0)

	if err := ts.store.CreateReservation(t.Context(), &store.Reservation{
		AccountID:  acct.ID,
		ClientID:   &client.ID,
		TunnelType: store.TunnelTypeHTTP,
		Subdomain:  "fleet-node",
		Enabled:    true,
	}); err != nil {
		t.Fatalf("CreateReservation() error = %v", err)
	}

	resp := registerOK(t, ts, protocol.RegisterRequest{
		Token:      token,
		TunnelType: protocol.TunnelTypeHTTP,
		LocalPort:  8080,
	})

	if resp.Subdomain != "fleet-node" {
		t.Fatalf("Subdomain = %q, want fleet-node", resp.Subdomain)
	}
}

// A TCP reservation pins the public port, not just a name.
func TestReservedTCPPortIsAllocated(t *testing.T) {
	ts := newTestServer(t, true, false)
	token, acct, client := ts.seedAccountCredential(t, "acme", 0)

	const port = 38021
	if err := ts.store.CreateReservation(t.Context(), &store.Reservation{
		AccountID:  acct.ID,
		ClientID:   &client.ID,
		TunnelType: store.TunnelTypeTCP,
		TCPPort:    port,
		Enabled:    true,
	}); err != nil {
		t.Fatalf("CreateReservation() error = %v", err)
	}

	resp := registerOK(t, ts, protocol.RegisterRequest{
		Token:      token,
		TunnelType: protocol.TunnelTypeTCP,
		LocalPort:  5432,
	})

	if resp.Port != port {
		t.Fatalf("Port = %d, want %d", resp.Port, port)
	}
}

// A disabled reservation must not silently fall back to a random name: the
// operator turned it off deliberately.
func TestDisabledReservationIsRefused(t *testing.T) {
	ts := newTestServer(t, true, false)
	token, acct, client := ts.seedAccountCredential(t, "acme", 0)

	if err := ts.store.CreateReservation(t.Context(), &store.Reservation{
		AccountID:  acct.ID,
		ClientID:   &client.ID,
		TunnelType: store.TunnelTypeHTTP,
		Subdomain:  "switched-off",
	}); err != nil {
		t.Fatalf("CreateReservation() error = %v", err)
	}

	frame := ts.register(t, protocol.RegisterRequest{
		Token:           token,
		TunnelType:      protocol.TunnelTypeHTTP,
		CustomSubdomain: "switched-off",
		LocalPort:       8080,
	})

	if frame.Type != protocol.FrameTypeError {
		t.Fatalf("frame type = %s, want Error", frame.Type)
	}
}
