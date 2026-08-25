package hostinger

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/libdns/libdns"
)

// defaultTTL is applied when a record carries no usable TTL. libdns models TTL
// as a duration and documents that sub-second values exist to avoid a
// provider's default; the API takes whole seconds, so anything under one second
// lands here.
const defaultTTL = 300

// Provider talks to the Hostinger DNS API on behalf of libdns.
//
// One limitation is worth stating: the update request carries a content and
// nothing else, so an RRset that mixes enabled and disabled contents cannot be
// rewritten with the disabled flags intact. DeleteRecords preserves the
// surviving contents but re-enables any disabled ones in that RRset. Drip only
// ever writes `_acme-challenge` TXT records through this provider, where the
// case does not arise.
type Provider struct {
	// APIToken is a Hostinger API token with DNS access to the zone, generated
	// in hPanel under API. Tokens carry an expiry date chosen at creation:
	// pick one past the certificate lifetime, or renewal starts failing with
	// HTTP 401 long after the token was set up and forgotten.
	APIToken string `json:"api_token,omitempty"`

	// BaseURL overrides the API endpoint. Empty uses the public API; tests
	// point it at a local server.
	BaseURL string `json:"-"`

	// HTTPClient overrides the client used for API calls.
	HTTPClient *http.Client `json:"-"`

	// mu serializes DeleteRecords. That call is a read-modify-write against an
	// API that can only rewrite a whole RRset, so two concurrent deletes on the
	// same name would race and one would resurrect the other's records. libdns
	// requires implementations to be safe for concurrent use, and certmagic
	// solves challenges for several names in parallel.
	mu sync.Mutex
}

var (
	_ libdns.RecordGetter   = (*Provider)(nil)
	_ libdns.RecordAppender = (*Provider)(nil)
	_ libdns.RecordSetter   = (*Provider)(nil)
	_ libdns.RecordDeleter  = (*Provider)(nil)
)

// GetRecords lists the records in the zone.
//
// Disabled contents are left out: they are stored but not served, so reporting
// them as live records would misrepresent the zone.
func (p *Provider) GetRecords(ctx context.Context, zone string) ([]libdns.Record, error) {
	entries, err := p.fetchZone(ctx, zone)
	if err != nil {
		return nil, err
	}

	var records []libdns.Record
	for _, entry := range entries {
		for _, content := range entry.Records {
			if content.IsDisabled {
				continue
			}
			records = append(records, parseRR(entry, content.Content))
		}
	}
	return records, nil
}

// AppendRecords adds records without touching what is already there.
func (p *Provider) AppendRecords(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	return p.write(ctx, zone, recs, false)
}

// SetRecords makes the given records the only members of their RRsets.
func (p *Provider) SetRecords(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	return p.write(ctx, zone, recs, true)
}

// write groups the records into RRsets and PUTs them. overwrite false appends
// contents to any existing RRset, true replaces it.
func (p *Provider) write(ctx context.Context, zone string, recs []libdns.Record, overwrite bool) ([]libdns.Record, error) {
	if len(recs) == 0 {
		return nil, nil
	}

	entries, err := groupRecords(zone, recs)
	if err != nil {
		return nil, err
	}

	if err := p.updateZone(ctx, zone, updateRequest{Overwrite: overwrite, Zone: entries}); err != nil {
		return nil, err
	}

	// Report what was stored rather than echoing the input: grouping settles a
	// single TTL per RRset, which may not be the one a given record asked for.
	var written []libdns.Record
	for _, entry := range entries {
		for _, content := range entry.Records {
			written = append(written, parseRR(entry, content.Content))
		}
	}
	return written, nil
}

// DeleteRecords removes records that exactly match the input.
//
// The API only deletes whole (name, type) RRsets, so anything short of that is
// a read-modify-write: fetch the zone, drop the matching contents, and either
// rewrite the RRset with the survivors or delete it outright when nothing is
// left.
func (p *Provider) DeleteRecords(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	if len(recs) == 0 {
		return nil, nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	entries, err := p.fetchZone(ctx, zone)
	if err != nil {
		return nil, err
	}

	targets := make([]libdns.RR, 0, len(recs))
	for _, rec := range recs {
		rr := rec.RR()
		rr.Name = normalizeName(rr.Name, zone)
		rr.Type = strings.ToUpper(strings.TrimSpace(rr.Type))
		targets = append(targets, rr)
	}

	var (
		deleted  []libdns.Record
		rewrites []zoneEntry
		drops    []destroyFilter
	)

	for _, entry := range entries {
		kept := make([]zoneContent, 0, len(entry.Records))
		for _, content := range entry.Records {
			if matchesAny(targets, entry, content.Content) {
				deleted = append(deleted, parseRR(entry, content.Content))
				continue
			}
			kept = append(kept, content)
		}

		switch {
		case len(kept) == len(entry.Records):
			// Nothing matched; leave the RRset alone.
		case len(kept) == 0:
			drops = append(drops, destroyFilter{Name: entry.Name, Type: entry.Type})
		default:
			survivors := make([]zoneContent, 0, len(kept))
			for _, content := range kept {
				survivors = append(survivors, zoneContent{Content: content.Content})
			}
			rewrites = append(rewrites, zoneEntry{
				Name:    entry.Name,
				Type:    entry.Type,
				TTL:     entry.TTL,
				Records: survivors,
			})
		}
	}

	if len(deleted) == 0 {
		return nil, nil
	}

	// Rewrite first. A failed rewrite leaves the zone untouched, whereas a
	// failed rewrite after a successful delete would leave records missing.
	if len(rewrites) > 0 {
		if err := p.updateZone(ctx, zone, updateRequest{Overwrite: true, Zone: rewrites}); err != nil {
			return nil, err
		}
	}
	if len(drops) > 0 {
		if err := p.destroyEntries(ctx, zone, drops); err != nil {
			return nil, err
		}
	}

	return deleted, nil
}

// matchesAny reports whether any target selects this content.
//
// libdns treats an empty Type, a zero TTL and an empty Data as wildcards, so a
// caller can delete by name alone. Name is never a wildcard.
func matchesAny(targets []libdns.RR, entry zoneEntry, content string) bool {
	for _, target := range targets {
		if target.Name != entry.Name {
			continue
		}
		if target.Type != "" && target.Type != strings.ToUpper(entry.Type) {
			continue
		}
		if target.TTL > 0 && int(target.TTL.Seconds()) != entry.TTL {
			continue
		}
		if target.Data != "" && target.Data != content {
			continue
		}
		return true
	}
	return false
}

// groupRecords collapses records into the RRsets the API expects.
//
// A single TTL covers an RRset, so a group takes the smallest positive TTL any
// of its records asked for: honouring the shortest keeps a record from being
// cached longer than requested, which matters for the challenge records this
// provider exists to write.
func groupRecords(zone string, recs []libdns.Record) ([]zoneEntry, error) {
	type key struct{ name, rrtype string }

	index := make(map[key]int, len(recs))
	entries := make([]zoneEntry, 0, len(recs))

	for _, rec := range recs {
		rr := rec.RR()

		name := normalizeName(rr.Name, zone)
		rrtype := strings.ToUpper(strings.TrimSpace(rr.Type))
		if rrtype == "" {
			return nil, fmt.Errorf("the %q record has no type", name)
		}
		if rr.Data == "" {
			return nil, fmt.Errorf("the %s %s record has no value", name, rrtype)
		}

		ttl := int(rr.TTL.Seconds())

		k := key{name: name, rrtype: rrtype}
		if at, ok := index[k]; ok {
			entries[at].Records = append(entries[at].Records, zoneContent{Content: rr.Data})
			if ttl > 0 && (entries[at].TTL <= 0 || ttl < entries[at].TTL) {
				entries[at].TTL = ttl
			}
			continue
		}

		index[k] = len(entries)
		entries = append(entries, zoneEntry{
			Name:    name,
			Type:    rrtype,
			TTL:     ttl,
			Records: []zoneContent{{Content: rr.Data}},
		})
	}

	for i := range entries {
		if entries[i].TTL <= 0 {
			entries[i].TTL = defaultTTL
		}
	}
	return entries, nil
}

// normalizeName renders a record name the way the API stores it: relative to
// the zone, with "@" for the apex.
func normalizeName(name, zone string) string {
	name = strings.TrimSpace(name)
	if name == "" || name == "@" {
		return "@"
	}
	if relative := libdns.RelativeName(name, zone); relative != "" {
		return relative
	}
	return "@"
}

// parseRR builds the typed libdns record for one content of an RRset, falling
// back to the opaque RR when the type is unknown or the value does not parse.
// A single odd record in a zone should not fail a whole listing, let alone
// certificate issuance.
func parseRR(entry zoneEntry, content string) libdns.Record {
	rr := libdns.RR{
		Name: entry.Name,
		Type: strings.ToUpper(entry.Type),
		TTL:  time.Duration(entry.TTL) * time.Second,
		Data: content,
	}
	parsed, err := rr.Parse()
	if err != nil {
		return rr
	}
	return parsed
}
