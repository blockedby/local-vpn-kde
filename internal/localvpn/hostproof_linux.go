package localvpn

import (
	"context"
	"errors"
	"net/netip"
	"strings"
)

var errNMNotReady = errors.New("owned NetworkManager tunnel is not ready")

func parseNMAddress(text string) (netip.Addr, error) {
	var address netip.Addr
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "--" {
			continue
		}
		if _, value, ok := strings.Cut(line, ":"); ok {
			line = strings.TrimSpace(value)
		}
		prefix, err := netip.ParsePrefix(line)
		if err != nil || !prefix.Addr().Is4() || address.IsValid() {
			return address, errors.New("invalid or ambiguous active IPv4 address")
		}
		address = prefix.Addr()
	}
	if !address.IsValid() {
		return address, errNMNotReady
	}
	return address, nil
}
func (n NetworkManager) activeDevice(ctx context.Context, uuid string) (string, error) {
	active, err := n.active(ctx, uuid)
	if err != nil {
		return "", err
	}
	if !active {
		return "", errNMNotReady
	}
	text, err := n.call(ctx, "-t", "-f", "IP4.ADDRESS", "connection", "show", "uuid", uuid)
	if err != nil {
		return "", err
	}
	address, err := parseNMAddress(text)
	if errors.Is(err, errNMNotReady) {
		fallback, e := n.call(ctx, "-t", "-f", "GENERAL.CON-UUID,IP4.ADDRESS", "device", "show")
		if e != nil {
			return "", errNMNotReady
		}
		matched := false
		values := []string{}
		for _, line := range strings.Split(fallback, "\n") {
			key, value, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			if strings.EqualFold(key, "GENERAL.CON-UUID") {
				matched = strings.EqualFold(value, uuid)
			} else if matched && strings.HasPrefix(key, "IP4.ADDRESS") {
				values = append(values, value)
			}
		}
		address, err = parseNMAddress(strings.Join(values, "\n"))
	}
	if err != nil {
		return "", err
	}
	data, err := hostCommand(ctx, "ip", "-o", "-4", "addr")
	if err != nil {
		return "", err
	}
	device := ""
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[2] != "inet" {
			continue
		}
		value := strings.SplitN(fields[3], "/", 2)[0]
		candidate, e := netip.ParseAddr(value)
		if e != nil || candidate != address {
			continue
		}
		if device != "" || !tunnelDevice.MatchString(fields[1]) {
			return "", errors.New("owned address does not identify exactly one safe TUN device")
		}
		device = fields[1]
	}
	if device == "" {
		return "", errors.New("owned address missing from kernel TUN interfaces")
	}
	return device, nil
}
func hostRoutes(ctx context.Context, device string) error {
	if !tunnelDevice.MatchString(device) {
		return errors.New("unsafe owned device")
	}
	for _, address := range []string{"1.1.1.1", "8.8.8.8"} {
		data, err := hostCommand(ctx, "ip", "-4", "route", "get", address)
		if err != nil {
			return errNMNotReady
		}
		if err := route4Device(data, address, device); err != nil {
			return err
		}
	}
	return nil
}

func route4Device(data []byte, address, device string) error {
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) > 2 || len(lines) == 2 && strings.TrimSpace(lines[1]) != "cache" {
		return errors.New("ambiguous IPv4 route")
	}
	fields := strings.Fields(lines[0])
	if len(fields) < 3 || fields[0] != address {
		return errors.New("invalid IPv4 route")
	}
	found := ""
	for i, field := range fields {
		if field == "unreachable" || field == "prohibit" || field == "blackhole" || field == "throw" {
			return errNMNotReady
		}
		if field == "dev" {
			if found != "" || i+1 >= len(fields) {
				return errors.New("ambiguous route device")
			}
			found = fields[i+1]
		}
	}
	if found == "" {
		return errors.New("route device missing")
	}
	if found != device {
		return errNMNotReady
	}
	return nil
}
