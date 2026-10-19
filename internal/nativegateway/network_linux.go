//go:build linux

package nativegateway

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

// These identifiers are deliberately fixed. A future executor must snapshot
// them before applying a plan and keep an ownership journal before attempting
// cleanup; the creation plan alone is not proof that a resource may be deleted.
const (
	RoutingTable = 21194
	// Precede Docker underlay rules (998+) without affecting host-originated traffic.
	EgressRulePriority      = 900
	UnreachableRulePriority = 901
	RouteProtocol           = 89
	NFTFamily               = "inet"
	NFTTable                = "vpnkit_native"
)

// Uplink is the non-VPN host path that namespace traffic is allowed to use.
// AddressCIDR is an address (not a network), for example 192.0.2.10/24.
type Uplink struct {
	Interface   string
	AddressCIDR string
	Gateway     string
}

// Command is inert plan data. Program is always selected by PlanNetwork;
// callers cannot supply programs, paths, or command fragments.
type Command struct {
	Program string
	Args    []string
	Stdin   string
}

type InterfaceState struct {
	Name      string
	Addresses []string
}

type RouteState struct {
	Table       int
	Destination string
	Gateway     string
	Interface   string
	Protocol    int
}

type RuleState struct {
	Priority          int
	From              string
	IncomingInterface string
	Table             int
	Action            string
	Protocol          int
}

type NFTTableState struct {
	Family string
	Name   string
}

// Snapshot contains the host facts needed to reject ambiguous ownership and
// address collisions. DockerGatewayActive is an explicit interlock for the
// overlapping legacy gateway; DockerNetworks catches overlap even when that
// gateway is observed indirectly.
type Snapshot struct {
	Interfaces          []InterfaceState
	Namespaces          []string
	Routes              []RouteState
	Rules               []RuleState
	NFTables            []NFTTableState
	DockerNetworks      []string
	DockerGatewayActive bool
	IPv4Forwarding      bool
}

var interfaceNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,14}$`)

// PlanNetwork validates a complete host snapshot and returns a creation-only
// command plan. It never probes or mutates the host. The plan intentionally has
// no teardown: safe deletion requires a durable ownership journal established
// by the eventual executor.
func PlanNetwork(uplink Uplink, snapshot Snapshot) ([]Command, error) {
	address, uplinkPrefix, gateway, err := validateUplink(uplink)
	if err != nil {
		return nil, err
	}
	transit := netip.MustParsePrefix(TransitCIDR)
	if prefixesOverlap(uplinkPrefix, transit) {
		return nil, errors.New("uplink overlaps native transit network")
	}
	if err := validateSnapshot(snapshot, transit); err != nil {
		return nil, err
	}

	table := strconv.Itoa(RoutingTable)
	protocol := strconv.Itoa(RouteProtocol)
	barrierPriority := strconv.Itoa(UnreachableRulePriority)
	egressPriority := strconv.Itoa(EgressRulePriority)
	hostCIDR := HostAddress + "/30"
	guestCIDR := GuestAddress + "/30"
	guestSource := GuestAddress + "/32"

	commands := []Command{
		{Program: "ip", Args: []string{"netns", "add", Namespace}},
		{Program: "ip", Args: []string{"link", "add", HostInterface, "type", "veth", "peer", "name", GuestInterface}},
		{Program: "ip", Args: []string{"link", "set", GuestInterface, "netns", Namespace}},

		// Install the fail-closed source rule before adding either a usable
		// egress lookup rule or bringing up the namespace transit.
		{Program: "ip", Args: []string{"rule", "add", "from", guestSource, "priority", barrierPriority, "protocol", protocol, "unreachable"}},
		{Program: "ip", Args: []string{"addr", "add", hostCIDR, "dev", HostInterface}},
		{Program: "ip", Args: []string{"-n", Namespace, "addr", "add", guestCIDR, "dev", GuestInterface}},
		{Program: "ip", Args: []string{"route", "add", uplinkPrefix.String(), "dev", uplink.Interface, "src", address.String(), "table", table, "protocol", protocol, "scope", "link"}},
		{Program: "ip", Args: []string{"route", "add", "default", "via", gateway.String(), "dev", uplink.Interface, "table", table, "protocol", protocol}},
		{Program: "ip", Args: []string{"rule", "add", "from", guestSource, "iif", HostInterface, "priority", egressPriority, "protocol", protocol, "lookup", table}},
		{Program: "nft", Args: []string{"-f", "-"}, Stdin: renderNFT(uplink.Interface)},
		forwardCompatibilityCommand(uplink.Interface, false, "-I"),
		forwardCompatibilityCommand(uplink.Interface, true, "-I"),
		{Program: "sysctl", Args: []string{"-w", "net.ipv4.conf." + HostInterface + ".route_localnet=1"}},
	}
	if !snapshot.IPv4Forwarding {
		commands = append(commands, Command{Program: "sysctl", Args: []string{"-w", "net.ipv4.ip_forward=1"}})
	}
	commands = append(commands,
		Command{Program: "ip", Args: []string{"netns", "exec", Namespace, "sysctl", "-w", "net.ipv6.conf.all.disable_ipv6=1"}},
		Command{Program: "ip", Args: []string{"netns", "exec", Namespace, "sysctl", "-w", "net.ipv6.conf.default.disable_ipv6=1"}},
		Command{Program: "ip", Args: []string{"link", "set", HostInterface, "up"}},
		Command{Program: "ip", Args: []string{"-n", Namespace, "link", "set", "lo", "up"}},
		Command{Program: "ip", Args: []string{"-n", Namespace, "link", "set", GuestInterface, "up"}},
		Command{Program: "ip", Args: []string{"-n", Namespace, "route", "add", "default", "via", HostAddress, "dev", GuestInterface}},
	)
	return commands, nil
}

func validateUplink(uplink Uplink) (netip.Addr, netip.Prefix, netip.Addr, error) {
	if !safeInterfaceName(uplink.Interface) || uplink.Interface == "lo" || uplink.Interface == HostInterface || uplink.Interface == GuestInterface {
		return netip.Addr{}, netip.Prefix{}, netip.Addr{}, errors.New("invalid uplink interface")
	}
	prefix, err := netip.ParsePrefix(uplink.AddressCIDR)
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() < 1 || prefix.Bits() > 30 || !usableIPv4(prefix.Addr()) {
		return netip.Addr{}, netip.Prefix{}, netip.Addr{}, errors.New("invalid uplink IPv4 CIDR")
	}
	address := prefix.Addr()
	network := prefix.Masked()
	if !usableHostInPrefix(network, address) {
		return netip.Addr{}, netip.Prefix{}, netip.Addr{}, errors.New("uplink CIDR uses a network or broadcast address")
	}
	gateway, err := netip.ParseAddr(uplink.Gateway)
	if err != nil || !gateway.Is4() || !usableIPv4(gateway) || !usableHostInPrefix(network, gateway) || gateway == address {
		return netip.Addr{}, netip.Prefix{}, netip.Addr{}, errors.New("invalid uplink gateway")
	}
	return address, network, gateway, nil
}

func validateSnapshot(snapshot Snapshot, transit netip.Prefix) error {
	if snapshot.DockerGatewayActive {
		return errors.New("legacy Docker gateway is active")
	}
	for _, namespace := range snapshot.Namespaces {
		if namespace == Namespace {
			return errors.New("native namespace already exists without proven ownership")
		}
	}
	for _, iface := range snapshot.Interfaces {
		if iface.Name == HostInterface || iface.Name == GuestInterface {
			return fmt.Errorf("native interface %q already exists without proven ownership", iface.Name)
		}
		for _, raw := range iface.Addresses {
			prefix, err := netip.ParsePrefix(raw)
			if err != nil {
				return fmt.Errorf("invalid interface address in snapshot: %q", raw)
			}
			if prefix.Addr().Is4() && prefixesOverlap(prefix.Masked(), transit) {
				return fmt.Errorf("interface %q overlaps native transit network", iface.Name)
			}
		}
	}
	for _, raw := range snapshot.DockerNetworks {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil {
			return fmt.Errorf("invalid Docker network in snapshot: %q", raw)
		}
		if prefix.Addr().Is4() && prefixesOverlap(prefix.Masked(), transit) {
			return errors.New("Docker network overlaps native transit network")
		}
	}
	for _, route := range snapshot.Routes {
		if route.Table == RoutingTable {
			return errors.New("owned routing table is already populated without proven ownership")
		}
		if route.Destination == "" || route.Destination == "default" {
			continue
		}
		prefix, err := netip.ParsePrefix(route.Destination)
		if err != nil {
			return fmt.Errorf("invalid route destination in snapshot: %q", route.Destination)
		}
		if prefix.Addr().Is4() && prefix.Bits() > 1 && prefixesOverlap(prefix.Masked(), transit) {
			return errors.New("existing route overlaps native transit network")
		}
	}
	for _, rule := range snapshot.Rules {
		if rule.Priority == EgressRulePriority || rule.Priority == UnreachableRulePriority || rule.Table == RoutingTable {
			return errors.New("owned policy routing identifier already exists without proven ownership")
		}
	}
	for _, table := range snapshot.NFTables {
		if table.Family == NFTFamily && table.Name == NFTTable {
			return errors.New("owned nftables table already exists without proven ownership")
		}
	}
	return nil
}

func safeInterfaceName(name string) bool {
	return interfaceNamePattern.MatchString(name)
}

func usableIPv4(address netip.Addr) bool {
	return address.Is4() && address.IsGlobalUnicast() && !address.IsLoopback() && !address.IsUnspecified() && !address.IsLinkLocalUnicast()
}

func usableHostInPrefix(prefix netip.Prefix, address netip.Addr) bool {
	if !prefix.Contains(address) || address == prefix.Addr() {
		return false
	}
	next := address.Next()
	return next.IsValid() && prefix.Contains(next)
}

func prefixesOverlap(a, b netip.Prefix) bool {
	return a.Addr().BitLen() == b.Addr().BitLen() && (a.Contains(b.Addr()) || b.Contains(a.Addr()))
}

func renderNFT(uplink string) string {
	q := strconv.Quote
	return strings.Join([]string{
		"table " + NFTFamily + " " + NFTTable + " {",
		"  chain output_nat {",
		"    type nat hook output priority -100; policy accept;",
		fmt.Sprintf("    oifname %s ip saddr 127.0.0.1 ip daddr 127.0.0.1 udp dport %d dnat ip to %s:%d", q("lo"), LoopbackPort, GuestAddress, GatewayPort),
		"  }",
		"  chain postrouting_nat {",
		"    type nat hook postrouting priority 100; policy accept;",
		fmt.Sprintf("    oifname %s ip saddr 127.0.0.1 ip daddr %s udp dport %d snat ip to %s", q(HostInterface), GuestAddress, GatewayPort, HostAddress),
		fmt.Sprintf("    iifname %s oifname %s ip saddr %s masquerade", q(HostInterface), q(uplink), GuestAddress+"/32"),
		"  }",
		"  chain forward {",
		"    type filter hook forward priority 0; policy accept;",
		fmt.Sprintf("    iifname %s oifname %s ip saddr %s ct state new,established,related accept", q(HostInterface), q(uplink), GuestAddress+"/32"),
		fmt.Sprintf("    iifname %s oifname %s ip daddr %s ct state established,related accept", q(uplink), q(HostInterface), GuestAddress+"/32"),
		fmt.Sprintf("    iifname %s drop", q(HostInterface)),
		fmt.Sprintf("    oifname %s drop", q(HostInterface)),
		"  }",
		"}",
		"",
	}, "\n")
}
