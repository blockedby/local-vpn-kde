package vless

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

type Node struct {
	Link, Name, Host, Network, Security string
	Port                                int
	// Outbound is the runtime outbound used for non-VLESS/static nodes, or the
	// legacy xray/V2Ray outbound for parsed VLESS subscription links.
	Outbound map[string]any
}

func Parse(link string) (Node, error) {
	link = strings.TrimSpace(link)
	u, err := url.Parse(link)
	if err != nil {
		return Node{}, err
	}
	if !strings.EqualFold(u.Scheme, "vless") {
		return Node{}, fmt.Errorf("not vless")
	}
	if u.User == nil || u.User.Username() == "" {
		return Node{}, fmt.Errorf("missing vless user id")
	}
	q := u.Query()
	host := u.Hostname()
	if host == "" {
		return Node{}, fmt.Errorf("missing host")
	}
	port := 80
	if p := u.Port(); p != "" {
		port, err = strconv.Atoi(p)
		if err != nil || port <= 0 || port > 65535 {
			return Node{}, fmt.Errorf("invalid port %q", p)
		}
	} else if q.Get("security") == "tls" || q.Get("security") == "reality" {
		port = 443
	}
	netw := q.Get("type")
	if netw == "" {
		netw = "tcp"
	}
	sec := q.Get("security")
	if sec == "" {
		sec = "none"
	}
	encryption := q.Get("encryption")
	if encryption == "" {
		encryption = "none"
	}
	user := map[string]any{"id": u.User.Username(), "encryption": encryption, "level": 0}
	if f := q.Get("flow"); f != "" {
		user["flow"] = f
	}
	ss := map[string]any{"network": netw, "security": sec}
	switch netw {
	case "ws":
		ws := map[string]any{"path": "/"}
		if p := q.Get("path"); p != "" {
			ws["path"] = p
		}
		if h := q.Get("host"); h != "" {
			ws["headers"] = map[string]any{"Host": h}
		}
		ss["wsSettings"] = ws
	case "grpc":
		ss["grpcSettings"] = map[string]any{"serviceName": first(q.Get("serviceName"), q.Get("service_name"))}
	}
	if sec == "tls" {
		m := map[string]any{"serverName": first(q.Get("sni"), q.Get("serverName"), host)}
		if fp := q.Get("fp"); fp != "" {
			m["fingerprint"] = fp
		}
		if alpn := splitCSV(q.Get("alpn")); len(alpn) > 0 {
			m["alpn"] = alpn
		}
		ss["tlsSettings"] = m
	}
	if sec == "reality" {
		publicKey := first(q.Get("pbk"), q.Get("publicKey"))
		if publicKey == "" {
			return Node{}, fmt.Errorf("reality link missing public key")
		}
		m := map[string]any{"serverName": first(q.Get("sni"), q.Get("serverName"), host), "fingerprint": first(q.Get("fp"), "chrome"), "publicKey": publicKey, "shortId": first(q.Get("sid"), q.Get("shortId"))}
		if spx := first(q.Get("spx"), q.Get("spiderX")); spx != "" {
			m["spiderX"] = spx
		}
		ss["realitySettings"] = m
	}
	ob := map[string]any{"protocol": "vless", "settings": map[string]any{"vnext": []any{map[string]any{"address": host, "port": port, "users": []any{user}}}}, "streamSettings": ss}
	name := u.Fragment
	if name == "" {
		name = fmt.Sprintf("%s:%d", host, port)
	}
	return Node{link, name, host, netw, sec, port, ob}, nil
}
func first(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
func (n Node) OutboundJSON() json.RawMessage { b, _ := json.Marshal(n.Outbound); return b }

// SingBoxOutbound converts a supported VLESS share link into a sing-box VLESS
// outbound. Node.Outbound intentionally remains the xray/V2Ray schema used by
// isolated benchmark tests and explicit xray runtime support.
func SingBoxOutbound(link string) (map[string]any, error) {
	n, err := Parse(link)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		return nil, err
	}
	q := u.Query()
	out := map[string]any{"type": "vless", "server": n.Host, "server_port": n.Port, "uuid": u.User.Username()}
	if flow := q.Get("flow"); flow != "" {
		out["flow"] = flow
	}
	switch n.Security {
	case "tls", "reality":
		tls := map[string]any{"enabled": true, "server_name": first(q.Get("sni"), q.Get("serverName"), n.Host)}
		if alpn := splitCSV(q.Get("alpn")); len(alpn) > 0 {
			tls["alpn"] = alpn
		}
		if fp := q.Get("fp"); fp != "" {
			tls["utls"] = map[string]any{"enabled": true, "fingerprint": fp}
		}
		if n.Security == "reality" {
			publicKey := first(q.Get("pbk"), q.Get("publicKey"))
			if publicKey == "" {
				return nil, fmt.Errorf("reality link missing public key")
			}
			tls["reality"] = map[string]any{"enabled": true, "public_key": publicKey, "short_id": first(q.Get("sid"), q.Get("shortId"))}
		}
		out["tls"] = tls
	case "none":
	default:
		return nil, fmt.Errorf("unsupported vless security %q", n.Security)
	}
	switch n.Network {
	case "tcp":
	case "ws":
		transport := map[string]any{"type": "ws", "path": first(q.Get("path"), "/")}
		if h := q.Get("host"); h != "" {
			transport["headers"] = map[string]any{"Host": h}
		}
		out["transport"] = transport
	case "grpc":
		out["transport"] = map[string]any{"type": "grpc", "service_name": first(q.Get("serviceName"), q.Get("service_name"))}
	default:
		return nil, fmt.Errorf("unsupported vless transport %q", n.Network)
	}
	return out, nil
}
