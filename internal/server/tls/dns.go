package tls

import (
	"fmt"
	"sort"
	"strings"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/cloudflare"
)

// DNSProviderConfig is the credential material a DNS provider needs to answer
// the ACME DNS-01 challenge.
type DNSProviderConfig struct {
	// Name selects the provider, e.g. "cloudflare".
	Name string
	// APIToken is the provider's scoped API token. Cloudflare needs a token
	// with Zone.DNS:Write on the zone hosting the tunnel domain.
	APIToken string
}

// dnsProviderFactory builds a certmagic DNS provider from credentials.
type dnsProviderFactory func(cfg DNSProviderConfig) (certmagic.DNSProvider, error)

// dnsProviders is the registry of supported DNS providers. Adding another
// libdns provider is a new import plus one entry here; the provider only has to
// satisfy libdns.RecordAppender and libdns.RecordDeleter.
var dnsProviders = map[string]dnsProviderFactory{
	"cloudflare": func(cfg DNSProviderConfig) (certmagic.DNSProvider, error) {
		if cfg.APIToken == "" {
			return nil, fmt.Errorf("cloudflare needs an API token with Zone.DNS:Write")
		}
		return &cloudflare.Provider{APIToken: cfg.APIToken}, nil
	},
}

// SupportedDNSProviders lists the registered provider names, sorted.
func SupportedDNSProviders() []string {
	names := make([]string, 0, len(dnsProviders))
	for name := range dnsProviders {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// newDNSProvider resolves a provider by name.
func newDNSProvider(cfg DNSProviderConfig) (certmagic.DNSProvider, error) {
	name := strings.ToLower(strings.TrimSpace(cfg.Name))
	if name == "" {
		return nil, fmt.Errorf("a DNS provider is required for ACME wildcard certificates: %s",
			strings.Join(SupportedDNSProviders(), ", "))
	}

	factory, ok := dnsProviders[name]
	if !ok {
		return nil, fmt.Errorf("unsupported DNS provider %q: want one of %s",
			cfg.Name, strings.Join(SupportedDNSProviders(), ", "))
	}

	cfg.Name = name
	return factory(cfg)
}
