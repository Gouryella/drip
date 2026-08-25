// Package hostinger implements the libdns interfaces on top of the Hostinger
// DNS API so the server can answer ACME DNS-01 challenges for zones hosted
// there.
//
// The API is RRset-oriented in a way libdns is not: a zone entry is a
// (name, type, ttl) tuple holding a list of contents, updates are a PUT of
// whole entries, and deletion only accepts (name, type) filters. libdns works
// one record at a time and requires exact-value deletes. The two are bridged by
// grouping records on write and by a read-modify-write on delete.
//
// Endpoints used, all under https://developers.hostinger.com:
//
//	GET    /api/dns/v1/zones/{domain}   list the zone
//	PUT    /api/dns/v1/zones/{domain}   create or update entries
//	DELETE /api/dns/v1/zones/{domain}   drop whole (name, type) RRsets
package hostinger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// defaultBaseURL is the public API endpoint.
	defaultBaseURL = "https://developers.hostinger.com"

	// zonePath is the DNS zone resource; the domain is appended escaped.
	zonePath = "/api/dns/v1/zones/"

	// maxResponseBody bounds the JSON a single call will decode. Zones are
	// small; a multi-megabyte body means something other than the API answered.
	maxResponseBody = 4 << 20

	// maxErrorBody bounds how much of a failure response is quoted back.
	maxErrorBody = 4 << 10

	// requestTimeout bounds one API call when no client is supplied. ACME
	// issuance already runs under a context deadline, but a provider that can
	// hang forever turns a slow API into a stuck server.
	requestTimeout = 30 * time.Second
)

// zoneEntry is one RRset as the API models it: a name and type carrying a
// single TTL and a list of contents.
type zoneEntry struct {
	Name    string        `json:"name"`
	Type    string        `json:"type"`
	TTL     int           `json:"ttl"`
	Records []zoneContent `json:"records"`
}

// zoneContent is one value within an RRset. IsDisabled is read-only in
// practice: the update request carries content alone.
type zoneContent struct {
	Content    string `json:"content"`
	IsDisabled bool   `json:"is_disabled,omitempty"`
}

// updateRequest is the PUT body. Overwrite false appends contents to the
// matching RRsets, true replaces them outright.
type updateRequest struct {
	Overwrite bool        `json:"overwrite"`
	Zone      []zoneEntry `json:"zone"`
}

// destroyRequest is the DELETE body. Filters match whole RRsets, never
// individual values.
type destroyRequest struct {
	Filters []destroyFilter `json:"filters"`
}

type destroyFilter struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

func (p *Provider) baseURL() string {
	if p.BaseURL != "" {
		return strings.TrimSuffix(p.BaseURL, "/")
	}
	return defaultBaseURL
}

func (p *Provider) client() *http.Client {
	if p.HTTPClient != nil {
		return p.HTTPClient
	}
	return &http.Client{Timeout: requestTimeout}
}

// zoneDomain turns a libdns zone into the domain path parameter. certmagic
// passes a fully-qualified zone with a trailing dot; the API wants neither.
func zoneDomain(zone string) (string, error) {
	domain := strings.TrimSuffix(strings.TrimSpace(zone), ".")
	if domain == "" {
		return "", errors.New("a DNS zone is required")
	}
	return domain, nil
}

// fetchZone lists the zone exactly as the API stores it, disabled contents
// included. Callers that hand records back to libdns filter those out; the
// delete path needs them so a rewrite does not drop them.
func (p *Provider) fetchZone(ctx context.Context, zone string) ([]zoneEntry, error) {
	domain, err := zoneDomain(zone)
	if err != nil {
		return nil, err
	}

	var entries []zoneEntry
	if err := p.do(ctx, http.MethodGet, zonePath+url.PathEscape(domain), nil, &entries); err != nil {
		return nil, fmt.Errorf("failed to list the %s DNS zone: %w", domain, err)
	}
	return entries, nil
}

// updateZone creates or replaces entries depending on overwrite.
func (p *Provider) updateZone(ctx context.Context, zone string, req updateRequest) error {
	domain, err := zoneDomain(zone)
	if err != nil {
		return err
	}
	if err := p.do(ctx, http.MethodPut, zonePath+url.PathEscape(domain), req, nil); err != nil {
		return fmt.Errorf("failed to update the %s DNS zone: %w", domain, err)
	}
	return nil
}

// destroyEntries drops whole RRsets.
func (p *Provider) destroyEntries(ctx context.Context, zone string, filters []destroyFilter) error {
	domain, err := zoneDomain(zone)
	if err != nil {
		return err
	}
	req := destroyRequest{Filters: filters}
	if err := p.do(ctx, http.MethodDelete, zonePath+url.PathEscape(domain), req, nil); err != nil {
		return fmt.Errorf("failed to delete records from the %s DNS zone: %w", domain, err)
	}
	return nil
}

// do performs one API call. A nil in skips the request body, a nil out
// discards the response body.
func (p *Provider) do(ctx context.Context, method, path string, in, out any) error {
	if p.APIToken == "" {
		return errors.New("no API token")
	}

	var body io.Reader
	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("failed to encode the request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, p.baseURL()+path, body)
	if err != nil {
		return fmt.Errorf("failed to build the request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.APIToken)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := p.client().Do(req)
	if err != nil {
		return fmt.Errorf("the request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return responseError(resp)
	}

	if out == nil {
		// Drain what is left so the connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
		return nil
	}

	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(out); err != nil {
		return fmt.Errorf("failed to decode the response: %w", err)
	}
	return nil
}

// responseError turns a non-2xx response into an error carrying the status and
// a bounded excerpt of the body, which is where the API puts validation
// details.
func responseError(resp *http.Response) error {
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	detail := strings.TrimSpace(string(payload))

	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("the API token was rejected (HTTP %d); it needs DNS access to the zone",
			resp.StatusCode)
	case http.StatusNotFound:
		return errors.New("the zone is not in this Hostinger account (HTTP 404)")
	}

	if detail == "" {
		return fmt.Errorf("the API returned HTTP %d", resp.StatusCode)
	}
	return fmt.Errorf("the API returned HTTP %d: %s", resp.StatusCode, detail)
}
