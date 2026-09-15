package localvpn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type ContainerConfig struct{ CIDR, Interface, Peer, Table, Generation, Owner, Runtime, Source, OpenVPN, Request, Ack, Vibe, Binary, IPv6 string }

func ContainerEnvironment() (ContainerConfig, error) {
	env := func(key, def string) string {
		if value := os.Getenv(key); value != "" {
			return value
		}
		return def
	}
	c := ContainerConfig{CIDR: env("OVPN_CIDR", "10.89.0.0/24"), Interface: env("SINGBOX_TUN_IFACE", "sb-tun0"), Peer: env("SINGBOX_TUN_PEER", "172.19.0.2"), Table: env("SINGBOX_TUN_TABLE", "101"), Generation: env("VPNKIT_SINGBOX_GENERATION_FILE", env("SINGBOX_GENERATION_FILE", "/run/vpnkit/sing-box-generation")), Owner: env("VPNKIT_FAIL_CLOSED_OWNER_FILE", "/run/vpnkit/fail-closed-chain.owner"), Runtime: env("SINGBOX_CONFIG", "/var/lib/vpnkit/sing-box/config.json"), Source: env("SINGBOX_SOURCE_CONFIG", "/etc/sing-box/config.json"), OpenVPN: env("OPENVPN_CONFIG", "/etc/openvpn/server.conf"), Request: env("SINGBOX_RESTART_FILE", "/run/vpnkit/restart-sing-box"), Vibe: env("VIBE_VPN_CONFIG", "/etc/vibe-vpn/config.yaml"), Binary: "/usr/local/bin/sing-box", IPv6: env("VPNKIT_IPV6_POLICY", "block")}
	c.Ack = env("SINGBOX_ACK_FILE", env("VPNKIT_SINGBOX_ACK_FILE", c.Generation+".ack"))
	if env("VPNKIT_ROUTING_MODE", "tun") != "tun" {
		return c, errors.New("local runtime requires TUN routing")
	}
	if env("OPENVPN_FAIL_CLOSED_CHAIN", "OVPN_FAIL_CLOSED") != "OVPN_FAIL_CLOSED" {
		return c, errors.New("fixed fail-closed chain required")
	}
	cidr, err := netip.ParsePrefix(c.CIDR)
	if err != nil || !cidr.Addr().Is4() || cidr.Bits() < 1 || cidr != cidr.Masked() {
		return c, errors.New("invalid OpenVPN subnet")
	}
	peer, err := netip.ParseAddr(c.Peer)
	if err != nil || !peer.Is4() {
		return c, errors.New("invalid TUN peer")
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{1,15}$`).MatchString(c.Interface) {
		return c, errors.New("invalid TUN interface")
	}
	table, err := strconv.ParseUint(c.Table, 10, 32)
	if err != nil || table < 1 || table == 253 || table == 254 || table == 255 {
		return c, errors.New("invalid routing table")
	}
	for _, path := range []string{c.Generation, c.Owner, c.Runtime, c.Source, c.OpenVPN, c.Request, c.Ack, c.Vibe} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return c, errors.New("invalid runtime path")
		}
	}
	if !strings.HasPrefix(c.Owner, "/run/") {
		return c, errors.New("owner marker must remain below /run")
	}
	if c.IPv6 != "block" && c.IPv6 != "allow" {
		return c, errors.New("invalid IPv6 policy")
	}
	return c, nil
}
func runtimeCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output := &boundedOutput{limit: 1024 * 1024, cancel: cancel}
	result := RunProcess(bounded, name, args, os.Environ(), output, nil, 250*time.Millisecond)
	data, overflow := output.result()
	if result.Reason != "ok" || overflow {
		return nil, fmt.Errorf("runtime command %s failed", filepath.Base(name))
	}
	return data, nil
}
func runtimeWrite(path string, data []byte) error {
	dir, err := directory(filepath.Dir(path), true, true)
	if err != nil {
		return err
	}
	defer unix.Close(dir)
	return atomicWrite(dir, filepath.Base(path), data)
}
func runtimeRead(path string) ([]byte, error) {
	dir, err := directory(filepath.Dir(path), false, false)
	if err != nil {
		return nil, err
	}
	defer unix.Close(dir)
	return readAt(dir, filepath.Base(path))
}
func runtimeRemove(path string) error {
	dir, err := directory(filepath.Dir(path), false, true)
	if err != nil {
		return err
	}
	defer unix.Close(dir)
	if _, err = readAt(dir, filepath.Base(path)); errors.Is(err, unix.ENOENT) {
		return nil
	} else if err != nil {
		return err
	}
	if err = unix.Unlinkat(dir, filepath.Base(path), 0); err != nil {
		return err
	}
	return unix.Fsync(dir)
}
func iptables(ctx context.Context, args ...string) error {
	_, err := runtimeCommand(ctx, "iptables", args...)
	return err
}
func ensureRule(ctx context.Context, binary, table, chain string, args ...string) error {
	prefix := []string{"-t", table, "-C", chain}
	if _, err := runtimeCommand(ctx, binary, append(prefix, args...)...); err == nil {
		return nil
	}
	_, err := runtimeCommand(ctx, binary, append([]string{"-t", table, "-A", chain}, args...)...)
	return err
}
func deleteRule(ctx context.Context, binary, table, chain string, args ...string) error {
	for n := 0; n < 256; n++ {
		if _, err := runtimeCommand(ctx, binary, append([]string{"-t", table, "-C", chain}, args...)...); err != nil {
			return nil
		}
		if _, err := runtimeCommand(ctx, binary, append([]string{"-t", table, "-D", chain}, args...)...); err != nil {
			return err
		}
	}
	return errors.New("too many duplicate rules")
}
func chainSnapshot(ctx context.Context) (string, error) {
	data, err := runtimeCommand(ctx, "iptables", "-t", "filter", "-S")
	return string(data), err
}
func chainDeclared(snapshot string) bool {
	for _, line := range strings.Split(snapshot, "\n") {
		if line == "-N OVPN_FAIL_CLOSED" {
			return true
		}
	}
	return false
}
func chainReferenced(snapshot string) bool {
	for _, line := range strings.Split(snapshot, "\n") {
		fields := strings.Fields(line)
		for i := 0; i+1 < len(fields); i++ {
			if (fields[i] == "-j" || fields[i] == "-g") && fields[i+1] == "OVPN_FAIL_CLOSED" {
				return true
			}
		}
	}
	return false
}
func chainPristine(snapshot string) bool {
	count := 0
	for _, line := range strings.Split(snapshot, "\n") {
		fields := strings.Fields(line)
		if line == "-N OVPN_FAIL_CLOSED" {
			count++
		}
		if len(fields) > 1 && fields[1] == "OVPN_FAIL_CLOSED" && (fields[0] == "-A" || fields[0] == "-I" || fields[0] == "-R") {
			return false
		}
	}
	return count == 1 && !chainReferenced(snapshot)
}
func (c ContainerConfig) ownsBarrier() bool {
	data, err := runtimeRead(c.Owner)
	return err == nil && strings.TrimSpace(string(data)) == "OVPN_FAIL_CLOSED"
}
func (c ContainerConfig) InstallBarrier(ctx context.Context) error {
	created := iptables(ctx, "-t", "filter", "-N", "OVPN_FAIL_CLOSED") == nil
	if !created && !c.ownsBarrier() {
		snapshot, err := chainSnapshot(ctx)
		if err != nil || !chainPristine(snapshot) {
			return errors.New("refusing unowned fail-closed chain")
		}
		created = true
	}
	if created {
		if err := runtimeWrite(c.Owner, []byte("OVPN_FAIL_CLOSED\n")); err != nil {
			return err
		}
	}
	if err := ensureRule(ctx, "iptables", "filter", "OVPN_FAIL_CLOSED", "-j", "DROP"); err != nil {
		return err
	}
	for _, chain := range []string{"INPUT", "FORWARD"} {
		if err := iptables(ctx, "-t", "filter", "-I", chain, "1", "-s", c.CIDR, "-j", "OVPN_FAIL_CLOSED"); err != nil {
			return err
		}
	}
	return nil
}
func (c ContainerConfig) RemoveBarrier(ctx context.Context) error {
	snapshot, err := chainSnapshot(ctx)
	if err != nil {
		return err
	}
	if !c.ownsBarrier() {
		if chainDeclared(snapshot) {
			return errors.New("refusing unowned barrier cleanup")
		}
		return nil
	}
	for _, chain := range []string{"INPUT", "FORWARD"} {
		if err = deleteRule(ctx, "iptables", "filter", chain, "-s", c.CIDR, "-j", "OVPN_FAIL_CLOSED"); err != nil {
			return err
		}
	}
	snapshot, err = chainSnapshot(ctx)
	if err != nil {
		return err
	}
	if chainDeclared(snapshot) {
		if chainReferenced(snapshot) {
			return errors.New("refusing referenced barrier cleanup")
		}
		if err = iptables(ctx, "-t", "filter", "-F", "OVPN_FAIL_CLOSED"); err != nil {
			return err
		}
		if err = iptables(ctx, "-t", "filter", "-X", "OVPN_FAIL_CLOSED"); err != nil {
			return err
		}
	}
	return runtimeRemove(c.Owner)
}
func (c ContainerConfig) ApplyRouting(ctx context.Context) error {
	if err := c.InstallBarrier(ctx); err != nil {
		return err
	}
	for _, setting := range []string{"net.ipv4.ip_forward=1", "net.ipv4.conf.all.src_valid_mark=1", "net.ipv4.conf.all.rp_filter=0", "net.ipv4.conf.default.rp_filter=0", "net.ipv4.conf.tun0.src_valid_mark=1", "net.ipv4.conf.tun0.rp_filter=0"} {
		_, _ = runtimeCommand(ctx, "sysctl", "-w", setting)
	}
	if err := c.ipv6Policy(ctx); err != nil {
		return err
	}
	if _, err := runtimeCommand(ctx, "ip", "link", "show", c.Interface); err != nil {
		return err
	}
	if _, err := runtimeCommand(ctx, "ip", "route", "replace", "default", "via", c.Peer, "dev", c.Interface, "table", c.Table); err != nil {
		return err
	}
	rules, err := runtimeCommand(ctx, "ip", "rule", "show")
	if err != nil {
		return err
	}
	if !c.hasSourceRule(string(rules), false) {
		if _, err = runtimeCommand(ctx, "ip", "rule", "add", "from", c.CIDR, "table", c.Table, "priority", "1000"); err != nil {
			return err
		}
	}
	if !c.hasSourceRule(string(rules), true) {
		if _, err = runtimeCommand(ctx, "ip", "rule", "add", "from", c.CIDR, "priority", "1001", "unreachable"); err != nil {
			return err
		}
	}
	for _, rule := range c.forwardRules() {
		if err = ensureRule(ctx, "iptables", "filter", "FORWARD", rule...); err != nil {
			return err
		}
	}
	current := uint64(0)
	if data, e := runtimeRead(c.Generation); e == nil {
		current, _ = strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	} else if !errors.Is(e, unix.ENOENT) {
		return e
	}
	if current == ^uint64(0) {
		return errors.New("generation overflow")
	}
	if err = runtimeWrite(c.Generation, []byte(strconv.FormatUint(current+1, 10)+"\n")); err != nil {
		return err
	}
	return c.RemoveBarrier(ctx)
}
func (c ContainerConfig) forwardRules() [][]string {
	return [][]string{{"-i", "tun0", "-o", c.Interface, "-s", c.CIDR, "-j", "ACCEPT"}, {"-i", c.Interface, "-o", "tun0", "-d", c.CIDR, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"}}
}
func (c ContainerConfig) ipv6Policy(ctx context.Context) error {
	jumps := [][]string{{"INPUT", "-i", "tun0"}, {"FORWARD", "-i", "tun0"}, {"OUTPUT", "-o", "tun0"}, {"FORWARD", "-o", "tun0"}}
	if c.IPv6 == "allow" {
		for _, jump := range jumps {
			if err := deleteRule(ctx, "ip6tables", "filter", jump[0], append(jump[1:], "-j", "OVPN_IPV6_BLOCK")...); err != nil {
				return err
			}
		}
		_, _ = runtimeCommand(ctx, "ip6tables", "-t", "filter", "-F", "OVPN_IPV6_BLOCK")
		_, _ = runtimeCommand(ctx, "ip6tables", "-t", "filter", "-X", "OVPN_IPV6_BLOCK")
		return nil
	}
	if _, err := os.Stat("/proc/sys/net/ipv6"); os.IsNotExist(err) {
		return nil
	}
	_, _ = runtimeCommand(ctx, "ip6tables", "-t", "filter", "-N", "OVPN_IPV6_BLOCK")

	// Install DROP before linking the chain, so new references never see an
	// empty ACCEPT-equivalent chain during initial setup.
	if _, err := runtimeCommand(ctx, "ip6tables", "-t", "filter", "-I", "OVPN_IPV6_BLOCK", "1", "-j", "DROP"); err != nil {
		return err
	}
	for _, jump := range jumps {
		if err := ensureRule(ctx, "ip6tables", "filter", jump[0], append(jump[1:], "-j", "OVPN_IPV6_BLOCK")...); err != nil {
			return err
		}
	}
	return nil
}
func (c ContainerConfig) Health(ctx context.Context) error {
	for _, name := range []string{"openvpn", "sing-box"} {
		if _, err := runtimeCommand(ctx, "pgrep", "-x", name); err != nil {
			return err
		}
	}
	for _, iface := range []string{"tun0", c.Interface} {
		if _, err := runtimeCommand(ctx, "ip", "link", "show", iface); err != nil {
			return err
		}
	}
	if _, err := runtimeCommand(ctx, c.Binary, "check", "-c", c.Runtime); err != nil {
		return err
	}
	data, err := runtimeRead(c.Generation)
	if err != nil {
		return err
	}
	generation, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || generation < 1 {
		return errors.New("missing healthy generation")
	}
	snapshot, err := chainSnapshot(ctx)
	if err != nil {
		return err
	}
	if chainDeclared(snapshot) || chainReferenced(snapshot) {
		return errors.New("fail-closed barrier still installed")
	}
	rules, err := runtimeCommand(ctx, "ip", "rule", "show")
	if err != nil || !c.hasSourceRule(string(rules), false) || !c.hasSourceRule(string(rules), true) {
		return errors.New("TUN policy rule absent")
	}
	routes, err := runtimeCommand(ctx, "ip", "-j", "route", "show", "table", c.Table)
	if err != nil {
		return err
	}
	var entries []struct{ Dst, Dev string }
	if json.Unmarshal(routes, &entries) != nil {
		return errors.New("invalid TUN routes")
	}
	found := false
	for _, route := range entries {
		if route.Dst == "default" && route.Dev == c.Interface {
			found = true
		}
	}
	if !found {
		return errors.New("TUN default route absent")
	}
	for _, rule := range c.forwardRules() {
		if err = iptables(ctx, append([]string{"-C", "FORWARD"}, rule...)...); err != nil {
			return err
		}
	}
	conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", "127.0.0.1:2080")
	if err != nil {
		return errors.New("SOCKS inbound unavailable")
	}
	return conn.Close()
}

func (c ContainerConfig) hasSourceRule(snapshot string, unreachable bool) bool {
	for _, line := range strings.Split(snapshot, "\n") {
		fields := strings.Fields(line)
		if unreachable {
			if len(fields) == 4 && fields[0] == "1001:" && fields[1] == "from" && fields[2] == c.CIDR && fields[3] == "unreachable" {
				return true
			}
		} else {
			if len(fields) == 5 && fields[0] == "1000:" && fields[1] == "from" && fields[2] == c.CIDR && fields[3] == "lookup" && fields[4] == c.Table {
				return true
			}
		}
	}
	return false
}
