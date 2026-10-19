//go:build linux

package nativegateway

import (
	"reflect"
	"strings"
	"testing"
)

func validUplink() Uplink {
	return Uplink{Interface: "enp5s0", AddressCIDR: "192.0.2.10/24", Gateway: "192.0.2.1"}
}

func commandIndex(commands []Command, program string, args ...string) int {
	for i, command := range commands {
		if command.Program == program && reflect.DeepEqual(command.Args, args) {
			return i
		}
	}
	return -1
}

func nftCommand(t *testing.T, commands []Command) Command {
	t.Helper()
	for _, command := range commands {
		if command.Program == "nft" {
			return command
		}
	}
	t.Fatal("nft command absent")
	return Command{}
}

func TestPlanNetworkInstallsBarrierBeforeEgressAndActivation(t *testing.T) {
	commands, err := PlanNetwork(validUplink(), Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	barrier := commandIndex(commands, "ip", "rule", "add", "from", GuestAddress+"/32", "priority", "5001", "protocol", "89", "unreachable")
	lookup := commandIndex(commands, "ip", "rule", "add", "from", GuestAddress+"/32", "iif", HostInterface, "priority", "5000", "protocol", "89", "lookup", "21194")
	defaultRoute := commandIndex(commands, "ip", "route", "add", "default", "via", "192.0.2.1", "dev", "enp5s0", "table", "21194", "protocol", "89")
	guestUp := commandIndex(commands, "ip", "-n", Namespace, "link", "set", GuestInterface, "up")
	if barrier < 0 || lookup < 0 || defaultRoute < 0 || guestUp < 0 {
		t.Fatalf("required command missing: barrier=%d route=%d lookup=%d guest-up=%d", barrier, defaultRoute, lookup, guestUp)
	}
	if !(barrier < defaultRoute && barrier < lookup && lookup < guestUp) {
		t.Fatalf("unsafe command order: barrier=%d route=%d lookup=%d guest-up=%d", barrier, defaultRoute, lookup, guestUp)
	}
	guestDefault := commandIndex(commands, "ip", "-n", Namespace, "route", "add", "default", "via", HostAddress, "dev", GuestInterface)
	if guestDefault <= guestUp {
		t.Fatal("namespace default route must follow transit link activation")
	}
	if EgressRulePriority >= UnreachableRulePriority {
		t.Fatal("usable lookup must precede the unreachable fallback in final policy order")
	}
}

func TestPlanNetworkUsesDedicatedUplinkTable(t *testing.T) {
	commands, err := PlanNetwork(validUplink(), Snapshot{IPv4Forwarding: true})
	if err != nil {
		t.Fatal(err)
	}
	connected := commandIndex(commands, "ip", "route", "add", "192.0.2.0/24", "dev", "enp5s0", "src", "192.0.2.10", "table", "21194", "protocol", "89", "scope", "link")
	lookup := commandIndex(commands, "ip", "rule", "add", "from", GuestAddress+"/32", "iif", HostInterface, "priority", "5000", "protocol", "89", "lookup", "21194")
	if connected < 0 || lookup < 0 {
		t.Fatal("guest egress is not pinned to the dedicated physical-uplink table")
	}
	for _, command := range commands {
		joined := strings.Join(command.Args, " ")
		if command.Program == "sysctl" && (strings.Contains(joined, "conf.all.route_localnet") || strings.Contains(joined, "conf.default.route_localnet")) {
			t.Fatalf("planned global route_localnet change: %q", joined)
		}
		if command.Program == "sysctl" && joined == "-w net.ipv4.ip_forward=1" {
			t.Fatal("already-enabled forwarding should not be rewritten")
		}
	}
}

func TestPlanNetworkRendersRestrictedNFTablesRules(t *testing.T) {
	commands, err := PlanNetwork(validUplink(), Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	nft := nftCommand(t, commands)
	if !reflect.DeepEqual(nft.Args, []string{"-f", "-"}) {
		t.Fatalf("unexpected nft invocation: %#v", nft.Args)
	}
	required := []string{
		`type nat hook output priority -100`,
		`type nat hook postrouting priority 100`,
		`type filter hook forward priority 0`,
		`oifname "lo" ip saddr 127.0.0.1 ip daddr 127.0.0.1 udp dport 31194 dnat ip to 172.30.90.2:1194`,
		`oifname "vpnkit-host" ip saddr 127.0.0.1 ip daddr 172.30.90.2 udp dport 1194 snat ip to 172.30.90.1`,
		`iifname "vpnkit-host" oifname "enp5s0" ip saddr 172.30.90.2/32 masquerade`,
		`iifname "vpnkit-host" drop`,
		`oifname "vpnkit-host" drop`,
	}
	for _, fragment := range required {
		if !strings.Contains(nft.Stdin, fragment) {
			t.Errorf("nft plan missing %q\n%s", fragment, nft.Stdin)
		}
	}
	for _, forbidden := range []string{"flush ruleset", "0.0.0.0/0 masquerade", "tcp dport", "udp dport 1194 dnat"} {
		if strings.Contains(nft.Stdin, forbidden) {
			t.Errorf("nft plan contains over-broad fragment %q", forbidden)
		}
	}
}

func TestPlanNetworkDisablesIPv6OnlyInsideNamespace(t *testing.T) {
	commands, err := PlanNetwork(validUplink(), Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, command := range commands {
		joined := strings.Join(command.Args, " ")
		if strings.Contains(joined, "disable_ipv6=1") {
			seen++
			if command.Program != "ip" || len(command.Args) < 4 || strings.Join(command.Args[:3], " ") != "netns exec "+Namespace {
				t.Fatalf("IPv6 mutation escaped namespace: %#v", command)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("got %d namespace IPv6 settings, want 2", seen)
	}
}

func TestPlanNetworkRejectsInvalidInputs(t *testing.T) {
	tests := []struct {
		name     string
		uplink   Uplink
		snapshot Snapshot
	}{
		{name: "unsafe interface", uplink: Uplink{Interface: "eth0;down", AddressCIDR: "192.0.2.10/24", Gateway: "192.0.2.1"}},
		{name: "loopback uplink", uplink: Uplink{Interface: "lo", AddressCIDR: "192.0.2.10/24", Gateway: "192.0.2.1"}},
		{name: "IPv6 address", uplink: Uplink{Interface: "eth0", AddressCIDR: "2001:db8::2/64", Gateway: "2001:db8::1"}},
		{name: "loopback address", uplink: Uplink{Interface: "eth0", AddressCIDR: "127.0.0.2/8", Gateway: "127.0.0.1"}},
		{name: "network address", uplink: Uplink{Interface: "eth0", AddressCIDR: "192.0.2.0/24", Gateway: "192.0.2.1"}},
		{name: "broadcast address", uplink: Uplink{Interface: "eth0", AddressCIDR: "192.0.2.255/24", Gateway: "192.0.2.1"}},
		{name: "gateway outside prefix", uplink: Uplink{Interface: "eth0", AddressCIDR: "192.0.2.10/24", Gateway: "198.51.100.1"}},
		{name: "gateway network address", uplink: Uplink{Interface: "eth0", AddressCIDR: "192.0.2.10/24", Gateway: "192.0.2.0"}},
		{name: "gateway equals address", uplink: Uplink{Interface: "eth0", AddressCIDR: "192.0.2.10/24", Gateway: "192.0.2.10"}},
		{name: "uplink overlaps transit", uplink: Uplink{Interface: "eth0", AddressCIDR: "172.30.90.1/24", Gateway: "172.30.90.254"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if commands, err := PlanNetwork(test.uplink, test.snapshot); err == nil || commands != nil {
				t.Fatalf("PlanNetwork() = %#v, %v; want nil error result", commands, err)
			}
		})
	}
}

func TestPlanNetworkRejectsForeignResourceCollisions(t *testing.T) {
	tests := []struct {
		name     string
		snapshot Snapshot
	}{
		{name: "Docker interlock", snapshot: Snapshot{DockerGatewayActive: true}},
		{name: "Docker overlap", snapshot: Snapshot{DockerNetworks: []string{"172.30.90.0/30"}}},
		{name: "namespace", snapshot: Snapshot{Namespaces: []string{Namespace}}},
		{name: "host veth", snapshot: Snapshot{Interfaces: []InterfaceState{{Name: HostInterface}}}},
		{name: "guest veth", snapshot: Snapshot{Interfaces: []InterfaceState{{Name: GuestInterface}}}},
		{name: "interface address overlap", snapshot: Snapshot{Interfaces: []InterfaceState{{Name: "br0", Addresses: []string{"172.30.90.0/29"}}}}},
		{name: "route overlap", snapshot: Snapshot{Routes: []RouteState{{Table: 254, Destination: "172.30.90.2/32"}}}},
		{name: "routing table", snapshot: Snapshot{Routes: []RouteState{{Table: RoutingTable, Destination: "default"}}}},
		{name: "egress priority", snapshot: Snapshot{Rules: []RuleState{{Priority: EgressRulePriority}}}},
		{name: "barrier priority", snapshot: Snapshot{Rules: []RuleState{{Priority: UnreachableRulePriority}}}},
		{name: "rule table", snapshot: Snapshot{Rules: []RuleState{{Priority: 100, Table: RoutingTable}}}},
		{name: "nft table", snapshot: Snapshot{NFTables: []NFTTableState{{Family: NFTFamily, Name: NFTTable}}}},
		{name: "malformed observed route", snapshot: Snapshot{Routes: []RouteState{{Destination: "not-a-prefix"}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if commands, err := PlanNetwork(validUplink(), test.snapshot); err == nil || commands != nil {
				t.Fatalf("PlanNetwork() = %#v, %v; want collision rejection", commands, err)
			}
		})
	}
}
