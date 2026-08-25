package hostinger

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/libdns/libdns"
)

// call records one request the fake API received.
type call struct {
	method string
	path   string
	auth   string
	body   json.RawMessage
}

// fakeAPI is a stand-in for the Hostinger DNS API. zone is what GET returns;
// calls accumulates every request so tests can assert on the wire format.
type fakeAPI struct {
	t      *testing.T
	zone   []zoneEntry
	calls  []call
	status int
	body   string
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.t.Helper()

	payload, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Fatalf("failed to read the request body: %v", err)
	}
	f.calls = append(f.calls, call{
		method: r.Method,
		path:   r.URL.Path,
		auth:   r.Header.Get("Authorization"),
		body:   payload,
	})

	if f.status != 0 {
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, f.body)
		return
	}

	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(f.zone); err != nil {
			f.t.Fatalf("failed to encode the zone: %v", err)
		}
		return
	}

	_, _ = io.WriteString(w, `{"message":"Success"}`)
}

func newProvider(t *testing.T, api *fakeAPI) *Provider {
	t.Helper()

	server := httptest.NewServer(api)
	t.Cleanup(server.Close)

	return &Provider{APIToken: "token", BaseURL: server.URL, HTTPClient: server.Client()}
}

func decodeUpdate(t *testing.T, raw json.RawMessage) updateRequest {
	t.Helper()

	var req updateRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("failed to decode the update request: %v", err)
	}
	return req
}

func TestAppendRecordsGroupsValuesIntoOneRRset(t *testing.T) {
	api := &fakeAPI{t: t}
	provider := newProvider(t, api)

	records := []libdns.Record{
		libdns.TXT{Name: "_acme-challenge.tunnel", TTL: 120 * time.Second, Text: "first"},
		libdns.TXT{Name: "_acme-challenge.tunnel", TTL: 60 * time.Second, Text: "second"},
	}

	written, err := provider.AppendRecords(context.Background(), "example.com.", records)
	if err != nil {
		t.Fatalf("AppendRecords() error = %v", err)
	}
	if len(written) != 2 {
		t.Fatalf("AppendRecords() returned %d records, want 2", len(written))
	}

	if len(api.calls) != 1 {
		t.Fatalf("AppendRecords() made %d calls, want 1", len(api.calls))
	}
	if api.calls[0].method != http.MethodPut {
		t.Errorf("method = %s, want PUT", api.calls[0].method)
	}
	if want := "/api/dns/v1/zones/example.com"; api.calls[0].path != want {
		t.Errorf("path = %s, want %s", api.calls[0].path, want)
	}
	if want := "Bearer token"; api.calls[0].auth != want {
		t.Errorf("Authorization = %q, want %q", api.calls[0].auth, want)
	}

	req := decodeUpdate(t, api.calls[0].body)
	if req.Overwrite {
		t.Error("overwrite = true, want false so existing records survive")
	}
	if len(req.Zone) != 1 {
		t.Fatalf("zone carried %d entries, want the two values grouped into 1", len(req.Zone))
	}
	entry := req.Zone[0]
	if entry.Name != "_acme-challenge.tunnel" || entry.Type != "TXT" {
		t.Errorf("entry = %s %s, want _acme-challenge.tunnel TXT", entry.Name, entry.Type)
	}
	// The shortest TTL wins so nothing is cached longer than it asked for.
	if entry.TTL != 60 {
		t.Errorf("ttl = %d, want 60", entry.TTL)
	}
	if len(entry.Records) != 2 {
		t.Fatalf("entry carried %d contents, want 2", len(entry.Records))
	}
	if entry.Records[0].Content != "first" || entry.Records[1].Content != "second" {
		t.Errorf("contents = %q, %q, want first, second",
			entry.Records[0].Content, entry.Records[1].Content)
	}
}

func TestAppendRecordsAppliesADefaultTTL(t *testing.T) {
	api := &fakeAPI{t: t}
	provider := newProvider(t, api)

	_, err := provider.AppendRecords(context.Background(), "example.com.",
		[]libdns.Record{libdns.TXT{Name: "@", Text: "value"}})
	if err != nil {
		t.Fatalf("AppendRecords() error = %v", err)
	}

	req := decodeUpdate(t, api.calls[0].body)
	if req.Zone[0].TTL != defaultTTL {
		t.Errorf("ttl = %d, want the %d default", req.Zone[0].TTL, defaultTTL)
	}
	if req.Zone[0].Name != "@" {
		t.Errorf("name = %q, want the apex marker @", req.Zone[0].Name)
	}
}

func TestSetRecordsOverwritesTheRRset(t *testing.T) {
	api := &fakeAPI{t: t}
	provider := newProvider(t, api)

	_, err := provider.SetRecords(context.Background(), "example.com.",
		[]libdns.Record{libdns.TXT{Name: "www", TTL: time.Minute, Text: "only"}})
	if err != nil {
		t.Fatalf("SetRecords() error = %v", err)
	}

	req := decodeUpdate(t, api.calls[0].body)
	if !req.Overwrite {
		t.Error("overwrite = false, want true so the RRset is replaced")
	}
}

func TestAppendRecordsRejectsARecordWithoutAType(t *testing.T) {
	api := &fakeAPI{t: t}
	provider := newProvider(t, api)

	_, err := provider.AppendRecords(context.Background(), "example.com.",
		[]libdns.Record{libdns.RR{Name: "www", Data: "value"}})
	if err == nil {
		t.Fatal("AppendRecords() with no record type = nil error, want failure")
	}
	if len(api.calls) != 0 {
		t.Errorf("AppendRecords() made %d calls, want none before validation passes", len(api.calls))
	}
}

func TestGetRecordsSkipsDisabledContents(t *testing.T) {
	api := &fakeAPI{t: t, zone: []zoneEntry{{
		Name: "www",
		Type: "TXT",
		TTL:  300,
		Records: []zoneContent{
			{Content: "live"},
			{Content: "parked", IsDisabled: true},
		},
	}}}
	provider := newProvider(t, api)

	records, err := provider.GetRecords(context.Background(), "example.com.")
	if err != nil {
		t.Fatalf("GetRecords() error = %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("GetRecords() returned %d records, want only the enabled one", len(records))
	}

	txt, ok := records[0].(libdns.TXT)
	if !ok {
		t.Fatalf("GetRecords() returned %T, want libdns.TXT", records[0])
	}
	if txt.Text != "live" {
		t.Errorf("text = %q, want live", txt.Text)
	}
	if txt.TTL != 300*time.Second {
		t.Errorf("ttl = %s, want 5m", txt.TTL)
	}
}

func TestDeleteRecordsRewritesAnRRsetItOnlyPartlyMatches(t *testing.T) {
	api := &fakeAPI{t: t, zone: []zoneEntry{{
		Name:    "_acme-challenge",
		Type:    "TXT",
		TTL:     120,
		Records: []zoneContent{{Content: "keep"}, {Content: "drop"}},
	}}}
	provider := newProvider(t, api)

	deleted, err := provider.DeleteRecords(context.Background(), "example.com.",
		[]libdns.Record{libdns.TXT{Name: "_acme-challenge", TTL: 120 * time.Second, Text: "drop"}})
	if err != nil {
		t.Fatalf("DeleteRecords() error = %v", err)
	}
	if len(deleted) != 1 {
		t.Fatalf("DeleteRecords() reported %d records, want 1", len(deleted))
	}

	if len(api.calls) != 2 {
		t.Fatalf("DeleteRecords() made %d calls, want a GET and a PUT", len(api.calls))
	}
	if api.calls[1].method != http.MethodPut {
		t.Fatalf("second call = %s, want PUT rather than a whole-RRset delete", api.calls[1].method)
	}

	req := decodeUpdate(t, api.calls[1].body)
	if !req.Overwrite {
		t.Error("overwrite = false, want true so the survivors replace the RRset")
	}
	if len(req.Zone) != 1 || len(req.Zone[0].Records) != 1 {
		t.Fatalf("rewrite = %+v, want one entry holding one content", req.Zone)
	}
	if req.Zone[0].Records[0].Content != "keep" {
		t.Errorf("survivor = %q, want keep", req.Zone[0].Records[0].Content)
	}
	if req.Zone[0].TTL != 120 {
		t.Errorf("ttl = %d, want the stored 120", req.Zone[0].TTL)
	}
}

func TestDeleteRecordsDropsAnRRsetItFullyMatches(t *testing.T) {
	api := &fakeAPI{t: t, zone: []zoneEntry{{
		Name:    "_acme-challenge",
		Type:    "TXT",
		TTL:     120,
		Records: []zoneContent{{Content: "only"}},
	}}}
	provider := newProvider(t, api)

	deleted, err := provider.DeleteRecords(context.Background(), "example.com.",
		[]libdns.Record{libdns.TXT{Name: "_acme-challenge", Text: "only"}})
	if err != nil {
		t.Fatalf("DeleteRecords() error = %v", err)
	}
	if len(deleted) != 1 {
		t.Fatalf("DeleteRecords() reported %d records, want 1", len(deleted))
	}

	if len(api.calls) != 2 {
		t.Fatalf("DeleteRecords() made %d calls, want a GET and a DELETE", len(api.calls))
	}
	if api.calls[1].method != http.MethodDelete {
		t.Fatalf("second call = %s, want DELETE", api.calls[1].method)
	}

	var req destroyRequest
	if err := json.Unmarshal(api.calls[1].body, &req); err != nil {
		t.Fatalf("failed to decode the destroy request: %v", err)
	}
	if len(req.Filters) != 1 {
		t.Fatalf("filters = %+v, want one", req.Filters)
	}
	if req.Filters[0].Name != "_acme-challenge" || req.Filters[0].Type != "TXT" {
		t.Errorf("filter = %+v, want _acme-challenge TXT", req.Filters[0])
	}
}

func TestDeleteRecordsLeavesTheZoneAloneWhenNothingMatches(t *testing.T) {
	api := &fakeAPI{t: t, zone: []zoneEntry{{
		Name:    "_acme-challenge",
		Type:    "TXT",
		TTL:     120,
		Records: []zoneContent{{Content: "other"}},
	}}}
	provider := newProvider(t, api)

	deleted, err := provider.DeleteRecords(context.Background(), "example.com.",
		[]libdns.Record{libdns.TXT{Name: "_acme-challenge", Text: "absent"}})
	if err != nil {
		t.Fatalf("DeleteRecords() error = %v", err)
	}
	if len(deleted) != 0 {
		t.Fatalf("DeleteRecords() reported %d records, want none", len(deleted))
	}
	if len(api.calls) != 1 {
		t.Fatalf("DeleteRecords() made %d calls, want the GET alone", len(api.calls))
	}
}

func TestDeleteRecordsTreatsEmptyFieldsAsWildcards(t *testing.T) {
	api := &fakeAPI{t: t, zone: []zoneEntry{
		{Name: "www", Type: "TXT", TTL: 120, Records: []zoneContent{{Content: "a"}, {Content: "b"}}},
		{Name: "www", Type: "A", TTL: 120, Records: []zoneContent{{Content: "203.0.113.1"}}},
		{Name: "other", Type: "TXT", TTL: 120, Records: []zoneContent{{Content: "untouched"}}},
	}}
	provider := newProvider(t, api)

	deleted, err := provider.DeleteRecords(context.Background(), "example.com.",
		[]libdns.Record{libdns.RR{Name: "www"}})
	if err != nil {
		t.Fatalf("DeleteRecords() error = %v", err)
	}
	if len(deleted) != 3 {
		t.Fatalf("DeleteRecords() reported %d records, want every record named www", len(deleted))
	}

	var req destroyRequest
	if err := json.Unmarshal(api.calls[1].body, &req); err != nil {
		t.Fatalf("failed to decode the destroy request: %v", err)
	}
	if len(req.Filters) != 2 {
		t.Fatalf("filters = %+v, want both www RRsets", req.Filters)
	}
}

func TestDeleteRecordsPreservesDisabledContents(t *testing.T) {
	api := &fakeAPI{t: t, zone: []zoneEntry{{
		Name: "www",
		Type: "TXT",
		TTL:  120,
		Records: []zoneContent{
			{Content: "drop"},
			{Content: "parked", IsDisabled: true},
		},
	}}}
	provider := newProvider(t, api)

	if _, err := provider.DeleteRecords(context.Background(), "example.com.",
		[]libdns.Record{libdns.TXT{Name: "www", Text: "drop"}}); err != nil {
		t.Fatalf("DeleteRecords() error = %v", err)
	}

	if api.calls[1].method != http.MethodPut {
		t.Fatalf("second call = %s, want a PUT that keeps the disabled content", api.calls[1].method)
	}
	req := decodeUpdate(t, api.calls[1].body)
	if len(req.Zone[0].Records) != 1 || req.Zone[0].Records[0].Content != "parked" {
		t.Errorf("rewrite = %+v, want the disabled content preserved", req.Zone[0].Records)
	}
}

func TestDeleteRecordsRelativizesFullyQualifiedNames(t *testing.T) {
	api := &fakeAPI{t: t, zone: []zoneEntry{{
		Name:    "_acme-challenge.tunnel",
		Type:    "TXT",
		TTL:     120,
		Records: []zoneContent{{Content: "value"}},
	}}}
	provider := newProvider(t, api)

	deleted, err := provider.DeleteRecords(context.Background(), "example.com.",
		[]libdns.Record{libdns.TXT{Name: "_acme-challenge.tunnel.example.com.", Text: "value"}})
	if err != nil {
		t.Fatalf("DeleteRecords() error = %v", err)
	}
	if len(deleted) != 1 {
		t.Fatalf("DeleteRecords() reported %d records, want 1", len(deleted))
	}
}

func TestAPIFailuresCarryTheStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, want: "HTTP 401"},
		{name: "missing zone", status: http.StatusNotFound, want: "HTTP 404"},
		{
			name:   "validation",
			status: http.StatusUnprocessableEntity,
			body:   `{"message":"The zone field is required."}`,
			want:   "The zone field is required.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			api := &fakeAPI{t: t, status: tt.status, body: tt.body}
			provider := newProvider(t, api)

			_, err := provider.GetRecords(context.Background(), "example.com.")
			if err == nil {
				t.Fatalf("GetRecords() against HTTP %d = nil error, want failure", tt.status)
			}
			if got := err.Error(); !strings.Contains(got, tt.want) {
				t.Errorf("error = %q, want it to mention %q", got, tt.want)
			}
		})
	}
}

func TestCallsWithoutATokenNeverReachTheAPI(t *testing.T) {
	api := &fakeAPI{t: t}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)

	provider := &Provider{BaseURL: server.URL, HTTPClient: server.Client()}
	if _, err := provider.GetRecords(context.Background(), "example.com."); err == nil {
		t.Fatal("GetRecords() without a token = nil error, want failure")
	}
	if len(api.calls) != 0 {
		t.Errorf("the API received %d calls, want none", len(api.calls))
	}
}

func TestZoneDomainStripsTheTrailingDot(t *testing.T) {
	t.Parallel()

	domain, err := zoneDomain(" example.com. ")
	if err != nil {
		t.Fatalf("zoneDomain() error = %v", err)
	}
	if domain != "example.com" {
		t.Errorf("zoneDomain() = %q, want example.com", domain)
	}
	if _, err := zoneDomain(" . "); err == nil {
		t.Error("zoneDomain() with an empty zone = nil error, want failure")
	}
}

func TestNormalizeName(t *testing.T) {
	t.Parallel()

	const zone = "example.com."

	tests := map[string]string{
		"":                       "@",
		"@":                      "@",
		"www":                    "www",
		"www.example.com":        "www",
		"www.example.com.":       "www",
		"example.com.":           "@",
		"_acme-challenge.tunnel": "_acme-challenge.tunnel",
	}

	for in, want := range tests {
		if got := normalizeName(in, zone); got != want {
			t.Errorf("normalizeName(%q) = %q, want %q", in, got, want)
		}
	}
}
