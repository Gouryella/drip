package protocol

import json "github.com/goccy/go-json"

// PoolCapabilities advertises client connection pool capabilities
type PoolCapabilities struct {
	MaxDataConns int `json:"max_data_conns"` // Maximum data connections client supports
	Version      int `json:"version"`        // Protocol version for pool features
}

// IPAccessControl defines IP-based access control rules for a tunnel
type IPAccessControl struct {
	AllowIPs []string `json:"allow_ips,omitempty"` // Allowed IPs or CIDR ranges (whitelist)
	DenyIPs  []string `json:"deny_ips,omitempty"`  // Denied IPs or CIDR ranges (blacklist)
}

// RegisterRequest is sent by client to register a tunnel
type RegisterRequest struct {
	Token           string     `json:"token"`            // Authentication token
	CustomSubdomain string     `json:"custom_subdomain"` // Optional custom subdomain
	TunnelType      TunnelType `json:"tunnel_type"`      // http, tcp, udp
	LocalPort       int        `json:"local_port"`       // Local port to forward to

	// Connection pool fields (optional, for multi-connection support)
	ConnectionType   string            `json:"connection_type,omitempty"`   // "primary" or empty for legacy
	TunnelID         string            `json:"tunnel_id,omitempty"`         // For data connections to join
	PoolCapabilities *PoolCapabilities `json:"pool_capabilities,omitempty"` // Client pool capabilities

	// Access control (optional)
	IPAccess *IPAccessControl `json:"ip_access,omitempty"` // IP-based access control rules
}

// RegisterResponse is sent by server after successful registration
type RegisterResponse struct {
	Subdomain string `json:"subdomain"`        // Assigned subdomain
	Port      int    `json:"port,omitempty"`   // Assigned TCP port (for TCP tunnels)
	URL       string `json:"url"`              // Full tunnel URL
	Message   string `json:"message"`          // Success message

	// Connection pool fields (optional, for multi-connection support)
	TunnelID         string `json:"tunnel_id,omitempty"`          // Unique tunnel identifier
	SupportsDataConn bool   `json:"supports_data_conn,omitempty"` // Server supports multi-connection
	RecommendedConns int    `json:"recommended_conns,omitempty"`  // Suggested data connection count
}

// DataConnectRequest is sent by data connections to join a tunnel
type DataConnectRequest struct {
	TunnelID     string `json:"tunnel_id"`     // Tunnel to join
	Token        string `json:"token"`         // Same auth token as primary
	ConnectionID string `json:"connection_id"` // Unique connection identifier
}

// DataConnectResponse acknowledges data connection
type DataConnectResponse struct {
	Accepted     bool   `json:"accepted"`              // Whether connection was accepted
	ConnectionID string `json:"connection_id"`         // Echoed connection ID
	Message      string `json:"message,omitempty"`     // Optional message
}

// ErrorMessage represents an error
type ErrorMessage struct {
	Code    string `json:"code"`    // Error code
	Message string `json:"message"` // Error message
}

// Marshal helpers for control plane messages (JSON encoding)
func MarshalJSON(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}

func UnmarshalJSON(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}
