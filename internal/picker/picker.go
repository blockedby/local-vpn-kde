package picker

import "github.com/blockedby/local-vpn-kde/internal/vless"

type NodeResult struct {
	Availability   string         `json:"availability,omitempty"`
	PingMS         int            `json:"ping_ms,omitempty"`
	PingStatus     string         `json:"ping_status,omitempty"`
	Index          int            `json:"index"`
	ServerID       string         `json:"server_id,omitempty"`
	Generation     uint64         `json:"generation,omitempty"`
	Name           string         `json:"name,omitempty"`
	Host           string         `json:"host,omitempty"`
	Network        string         `json:"network,omitempty"`
	Security       string         `json:"security,omitempty"`
	Error          string         `json:"error,omitempty"`
	Link           string         `json:"link,omitempty"`
	Port           int            `json:"port,omitempty"`
	Mbps           float64        `json:"mbps,omitempty"`
	Bytes          int64          `json:"bytes,omitempty"`
	Seconds        float64        `json:"seconds,omitempty"`
	OK             bool           `json:"ok"`
	Excluded       bool           `json:"excluded,omitempty"`
	ExcludeReasons []string       `json:"exclude_reasons,omitempty"`
	Outbound       map[string]any `json:"outbound,omitempty"`
}

func Best(rs []NodeResult) *NodeResult {
	var b *NodeResult
	for i := range rs {
		if rs[i].OK && (b == nil || rs[i].Mbps > b.Mbps) {
			b = &rs[i]
		}
	}
	return b
}
func FromNode(i int, n vless.Node, mbps float64, bytes int64, sec float64) NodeResult {
	return NodeResult{Index: i, Name: n.Name, Host: n.Host, Network: n.Network, Security: n.Security, Link: n.Link, Port: n.Port, Mbps: mbps, Bytes: bytes, Seconds: sec, OK: true, Outbound: n.Outbound}
}
