package localvpn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type SmokeOptions struct {
	Device, RouteIP, PingIPs, Hostname, IPv6 string
	Timeout                                  time.Duration
}

func SmokeEnvironment() (SmokeOptions, error) {
	get := func(key, fallback string) string {
		if value := os.Getenv("VPNKIT_LOCAL_SMOKE_" + key); value != "" {
			return value
		}
		return fallback
	}
	seconds, err := strconv.Atoi(get("TIMEOUT_SECONDS", "8"))
	return SmokeOptions{Device: get("DEVICE", ""), RouteIP: get("ROUTE_IP", "1.1.1.1"),
		PingIPs: get("PING_IPS", "1.1.1.1,8.8.8.8"), Hostname: get("HOSTNAME", "example.com"),
		IPv6: get("IPV6_ADDRESS", "2606:4700:4700::1111"), Timeout: time.Duration(seconds) * time.Second}, err
}

func canonicalIPv4(value string) (string, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 4 {
		return "", errors.New("invalid IPv4 address")
	}
	for i, part := range parts {
		if len(part) < 1 || len(part) > 3 || strings.Trim(part, "0123456789") != "" {
			return "", errors.New("invalid IPv4 address")
		}
		n, err := strconv.Atoi(part)
		if err != nil || n > 255 {
			return "", errors.New("invalid IPv4 address")
		}
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, "."), nil
}

// HostSmoke checks the route before each application probe. The hostname
// probe is pinned to the exact addresses checked, with proxies/config disabled.
func HostSmoke(ctx context.Context, o SmokeOptions, output io.Writer) (err error) {
	check := "inputs"
	defer func() {
		if err != nil {
			fmt.Fprintf(output, "host_smoke_failed_check=%s\n", check)
		}
	}()
	if !tunnelDevice.MatchString(o.Device) || o.Timeout < time.Second || o.Timeout > 30*time.Second {
		return errors.New("invalid smoke device or timeout")
	}
	o.RouteIP, err = canonicalIPv4(o.RouteIP)
	if err != nil {
		return err
	}
	v6, err := netip.ParseAddr(o.IPv6)
	if err != nil || !v6.Is6() || v6.Is4In6() || v6.Zone() != "" {
		return errors.New("invalid smoke IPv6 address")
	}
	if len(o.Hostname) > 253 || !regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?$`).MatchString(o.Hostname) || strings.Contains(o.Hostname, "..") {
		return errors.New("invalid smoke hostname")
	}
	pings := strings.Split(o.PingIPs, ",")
	if len(pings) > 32 {
		return errors.New("too many smoke ping targets")
	}
	for i, address := range pings {
		pings[i], err = canonicalIPv4(address)
		if err != nil {
			return err
		}
	}
	for _, tool := range []string{"ip", "getent", "curl", "ping"} {
		if _, err = exec.LookPath(tool); err != nil {
			return errors.New("required smoke tool unavailable")
		}
	}
	command := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return runHostCommand(ctx, o.Timeout, false, name, args...)
	}
	route := func(ctx context.Context, address string) error {
		data, err := command(ctx, "ip", "-4", "route", "get", address)
		if err != nil {
			return errors.New("IPv4 route lookup failed")
		}
		if err := route4Device(data, address, o.Device); err != nil {
			if errors.Is(err, errNMNotReady) {
				return errors.New("IPv4 route did not use the exact local VPN device")
			}
			return errors.New("IPv4 route lookup returned malformed output")
		}
		return nil
	}
	check = "route-policy"
	ready, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	for {
		if route(ready, "1.1.1.1") == nil && route(ready, "208.67.222.222") == nil {
			break
		}
		select {
		case <-ready.Done():
			return errors.New("IPv4 full-tunnel routes did not converge on the local VPN device")
		case <-time.After(100 * time.Millisecond):
		}
	}
	for _, address := range []string{"1.1.1.1", "208.67.222.222"} {
		if err = route(ctx, address); err != nil {
			return err
		}
	}
	check = "dns"
	resolved, err := command(ctx, "getent", "ahostsv4", o.Hostname)
	if err != nil {
		return errors.New("DNS hostname smoke failed")
	}
	addresses := []string{}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(resolved)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			return errors.New("DNS hostname smoke returned no addresses")
		}
		address, err := canonicalIPv4(fields[0])
		if err != nil {
			return errors.New("DNS hostname smoke returned an invalid address")
		}
		if !seen[address] {
			addresses = append(addresses, address)
			seen[address] = true
		}
	}
	if len(addresses) > 256 {
		return errors.New("too many DNS smoke addresses")
	}
	check = "dns-route"
	for _, address := range addresses {
		if err = route(ctx, address); err != nil {
			return err
		}
	}
	check = "literal-ip-route"
	if err = route(ctx, o.RouteIP); err != nil {
		return err
	}
	seconds := strconv.Itoa(int(o.Timeout / time.Second))
	curl := func(insecure bool, url string, extra ...string) error {
		flags := "-fsS"
		if insecure {
			flags = "-kfsS"
		}
		args := []string{"-q", "--noproxy", "*", "-4", flags, "--connect-timeout", seconds, "--max-time", seconds, "-o", "/dev/null"}
		args = append(args, extra...)
		_, err := command(ctx, "curl", append(args, url)...)
		return err
	}
	check = "literal-ip-https"
	if err = curl(true, "https://"+o.RouteIP+"/"); err != nil {
		return errors.New("literal-IP HTTPS smoke failed")
	}
	check = "hostname-https"
	if err = curl(false, "https://"+o.Hostname+"/", "--resolve", o.Hostname+":443:"+strings.Join(addresses, ",")); err != nil {
		return errors.New("hostname HTTPS smoke failed")
	}
	for _, address := range pings {
		check = "ping-route"
		if err = route(ctx, address); err != nil {
			return err
		}
		check = "ping"
		if _, err = command(ctx, "ping", "-4", "-c", "1", "-W", seconds, address); err != nil {
			return errors.New("IPv4 ping smoke failed")
		}
	}
	check = "ipv6"
	data, routeErr := command(ctx, "ip", "-6", "route", "get", o.IPv6)
	var exited *exec.ExitError
	if routeErr != nil && !errors.As(routeErr, &exited) {
		return errors.New("IPv6 route probe did not complete")
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		if routeErr == nil {
			return errors.New("IPv6 route lookup returned empty success")
		}
	} else {
		fields := strings.Fields(text)
		if strings.Contains(text, "\n") || len(fields) < 2 || fields[0] != "unreachable" && fields[0] != "prohibit" && fields[0] != "blackhole" {
			return errors.New("IPv6 route is unexpectedly available or malformed")
		}
	}
	_, pingErr := command(ctx, "ping", "-6", "-c", "1", "-W", seconds, o.IPv6)
	if pingErr == nil {
		return errors.New("IPv6 ping unexpectedly succeeded")
	}
	if !errors.As(pingErr, &exited) {
		return errors.New("IPv6 ping probe did not complete")
	}
	for _, name := range []string{"host_smoke", "route", "route_policy", "route_policy_lower", "route_policy_upper", "route_dns", "route_literal_ip", "route_ping", "dns", "hostname_https", "literal_ip_https", "ipv4_ping", "ipv6_block"} {
		fmt.Fprintf(output, "%s=pass\n", name)
	}
	return nil
}
