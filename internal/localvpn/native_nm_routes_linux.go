//go:build linux

package localvpn

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

func (n NetworkManager) routeMetric() string {
	if n.Native {
		return "0"
	}
	return "50"
}

func (n NetworkManager) preferNativeRoutes(ctx context.Context, uuid string) error {
	raw, err := n.call(ctx, "-g", "ipv4.route-metric", "connection", "show", "uuid", uuid)
	if err != nil {
		return err
	}
	metric, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || metric < -1 || metric > 1<<32-1 {
		return errors.New("invalid owned native route metric")
	}
	if metric == 0 {
		return nil
	}
	_, err = n.call(ctx, "connection", "modify", "uuid", uuid, "ipv4.route-metric", "0")
	return err
}

// Both full-tunnel halves must belong to the owned TUN. More-specific existing
// routes keep normal kernel precedence; no foreign profile or route is edited.
func nativeFullTunnelRoutes(ctx context.Context, device string) error {
	if !tunnelDevice.MatchString(device) {
		return errors.New("unsafe owned device")
	}
	data, err := hostCommand(ctx, "ip", "-json", "-4", "route", "show", "dev", device)
	if err != nil {
		return errors.New("native full-tunnel routes could not be read")
	}
	var routes []struct {
		Dst  string `json:"dst"`
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &routes) != nil {
		return errors.New("invalid native full-tunnel routes")
	}
	halves := map[string]bool{}
	for _, r := range routes {
		if r.Type != "" && r.Type != "unicast" {
			continue
		}
		halves[r.Dst] = true
	}
	if !halves["0.0.0.0/1"] || !halves["128.0.0.0/1"] {
		return errors.New("native full-tunnel split routes are missing")
	}
	for _, address := range []string{"1.1.1.1", "208.67.222.222"} {
		data, err = hostCommand(ctx, "ip", "-4", "route", "get", address)
		if err != nil || route4Device(data, address, device) != nil {
			return errors.New("native full-tunnel route priority conflicts with existing routing")
		}
	}
	return nil
}
