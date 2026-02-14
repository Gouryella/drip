package protocol

import json "github.com/goccy/go-json"

type PoolCapabilities struct {
	MaxDataConns int `json:"max_data_conns"`
	Version      int `json:"version"`
}

type IPAccessControl struct {
	AllowIPs []string `json:"allow_ips,omitempty"`
	DenyIPs  []string `json:"deny_ips,omitempty"`
}

type ProxyAuth struct {
	Enabled  bool   `json:"enabled"`
	Password string `json:"password,omitempty"`
}

type RegisterRequest struct {
	Token            string            `json:"token"`
	CustomSubdomain  string            `json:"custom_subdomain"`
	TunnelType       TunnelType        `json:"tunnel_type"`
	LocalPort        int               `json:"local_port"`
	ConnectionType   string            `json:"connection_type,omitempty"`
	TunnelID         string            `json:"tunnel_id,omitempty"`
	PoolCapabilities *PoolCapabilities `json:"pool_capabilities,omitempty"`
	IPAccess         *IPAccessControl  `json:"ip_access,omitempty"`
	ProxyAuth        *ProxyAuth        `json:"proxy_auth,omitempty"`
	Bandwidth        int64             `json:"bandwidth,omitempty"` // Bandwidth limit (bytes/sec), 0 = unlimited
}

type RegisterResponse struct {
	Subdomain        string `json:"subdomain"`
	Port             int    `json:"port,omitempty"`
	URL              string `json:"url"`
	Message          string `json:"message"`
	TunnelID         string `json:"tunnel_id,omitempty"`
	SupportsDataConn bool   `json:"supports_data_conn,omitempty"`
	RecommendedConns int    `json:"recommended_conns,omitempty"`
	Bandwidth        int64  `json:"bandwidth,omitempty"` // Applied bandwidth limit (bytes/sec)
}

type DataConnectRequest struct {
	TunnelID     string `json:"tunnel_id"`
	Token        string `json:"token"`
	ConnectionID string `json:"connection_id"`
}

type DataConnectResponse struct {
	Accepted     bool   `json:"accepted"`
	ConnectionID string `json:"connection_id"`
	Message      string `json:"message,omitempty"`
}

type ErrorMessage struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func MarshalJSON(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}

func UnmarshalJSON(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}
