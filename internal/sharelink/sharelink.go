// Package sharelink translates supported server URIs to the bundled sing-box schema.
package sharelink

import (
	"errors"
	"github.com/blockedby/local-vpn-kde/internal/vless"
	"net/url"
	"strconv"
	"strings"
)

func Supported(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.HasPrefix(s, "vless://") || strings.HasPrefix(s, "hysteria2://") || strings.HasPrefix(s, "hy2://")
}
func Parse(raw string) (vless.Node, error) {
	raw = strings.TrimSpace(raw)
	bad := errors.New("invalid or unsupported server link")
	if strings.HasPrefix(strings.ToLower(raw), "vless://") {
		n, e := vless.Parse(raw)
		if e != nil {
			return vless.Node{}, bad
		}
		return n, nil
	}
	u, e := url.Parse(raw)
	if e != nil || (u.Scheme != "hysteria2" && u.Scheme != "hy2") || u.Hostname() == "" || u.User == nil || u.User.Username() == "" || (u.Path != "" && u.Path != "/") {
		return vless.Node{}, bad
	}
	q, e := url.ParseQuery(u.RawQuery)
	if e != nil {
		return vless.Node{}, bad
	}
	for key, values := range q {
		if len(values) != 1 {
			return vless.Node{}, bad
		}
		switch key {
		case "sni", "insecure", "obfs", "obfs-password":
		default:
			return vless.Node{}, errors.New("unsupported Hysteria2 link option")
		}
	}
	port := 443
	if u.Port() != "" {
		port, e = strconv.Atoi(u.Port())
		if e != nil || port < 1 || port > 65535 {
			return vless.Node{}, bad
		}
	}
	password := u.User.Username()
	if p, ok := u.User.Password(); ok {
		password += ":" + p
	}
	tls := map[string]any{"enabled": true, "server_name": u.Hostname()}
	if q.Get("sni") != "" {
		tls["server_name"] = q.Get("sni")
	}
	switch q.Get("insecure") {
	case "", "0", "false":
	case "1", "true":
		tls["insecure"] = true
	default:
		return vless.Node{}, bad
	}
	outbound := map[string]any{"type": "hysteria2", "server": u.Hostname(), "server_port": port, "password": password, "tls": tls}
	if q.Get("obfs") != "" {
		if q.Get("obfs") != "salamander" || q.Get("obfs-password") == "" {
			return vless.Node{}, bad
		}
		outbound["obfs"] = map[string]any{"type": "salamander", "password": q.Get("obfs-password")}
	} else if q.Get("obfs-password") != "" {
		return vless.Node{}, bad
	}
	name := u.Fragment
	if name == "" {
		name = "Hysteria2"
	}
	return vless.Node{Link: raw, Name: name, Host: u.Hostname(), Port: port, Network: "quic", Security: "tls", Outbound: outbound}, nil
}
func Outbound(raw string) (map[string]any, error) {
	n, e := Parse(raw)
	if e != nil {
		return nil, e
	}
	if n.Outbound["type"] == "hysteria2" {
		return n.Outbound, nil
	}
	return vless.SingBoxOutbound(raw)
}
