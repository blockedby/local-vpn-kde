//go:build linux

package nativegateway

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

type inspectFS struct {
	stat    LinuxNamespaceStat
	statErr error
	files   map[string][]byte
}

func (f *inspectFS) StatNamespace(string) (LinuxNamespaceStat, error) { return f.stat, f.statErr }
func (f *inspectFS) ReadFile(path string) ([]byte, error) {
	value, ok := f.files[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return append([]byte(nil), value...), nil
}

func fullExpectation(t *testing.T, forwarding bool) NetworkExpectation {
	t.Helper()
	uplink := Uplink{Interface: "enp5s0", AddressCIDR: "192.0.2.10/24", Gateway: "192.0.2.1"}
	commands, err := PlanNetwork(uplink, Snapshot{IPv4Forwarding: forwarding})
	if err != nil {
		t.Fatal(err)
	}
	kinds, err := effectKinds(commands, forwarding)
	if err != nil {
		t.Fatal(err)
	}
	return expectation(uplink, forwarding, kinds)
}

func exactInspectFixture(t *testing.T) (map[string][]byte, *inspectFS) {
	t.Helper()
	nft, err := json.Marshal(expectedNFT("enp5s0"))
	if err != nil {
		t.Fatal(err)
	}
	outputs := map[string][]byte{
		commandKey("iptables", "-w", "5", "-S", "FORWARD"): []byte("-P FORWARD DROP\n" + strings.Join(append([]string{"-A", "FORWARD"}, forwardCompatibilityRule("enp5s0", false)...), " ") + "\n" + strings.Join(append([]string{"-A", "FORWARD"}, forwardCompatibilityRule("enp5s0", true)...), " ") + "\n"),
		commandKey("ip", "-json", "-details", "link", "show"): []byte(`[
			{"ifname":"lo","ifindex":1,"flags":["LOOPBACK","UP"]},
			{"ifname":"enp5s0","ifindex":2,"flags":["UP"]},
			{"ifname":"vpnkit-host","ifindex":10,"link_index":20,"flags":["BROADCAST","UP"],"linkinfo":{"info_kind":"veth"}}
		]`),
		commandKey("ip", "-json", "address", "show"): []byte(`[
			{"ifname":"lo","addr_info":[{"family":"inet","local":"127.0.0.1","prefixlen":8}]},
			{"ifname":"enp5s0","addr_info":[{"family":"inet","local":"192.0.2.10","prefixlen":24}]},
			{"ifname":"vpnkit-host","addr_info":[{"family":"inet","local":"172.30.90.1","prefixlen":30}]}
		]`),
		commandKey("ip", "-json", "-4", "route", "show", "table", "21194"): []byte(`[
			{"dst":"192.0.2.0/24","dev":"enp5s0","prefsrc":"192.0.2.10","scope":"link","table":21194,"protocol":89},
			{"dst":"default","gateway":"192.0.2.1","dev":"enp5s0","table":21194,"protocol":89}
		]`),
		commandKey("ip", "-json", "-4", "rule", "show"): []byte(`[
			{"priority":0,"src":"all","table":"local"},
			{"priority":5000,"src":"172.30.90.2/32","iif":"vpnkit-host","table":21194,"protocol":89,"action":"lookup"},
			{"priority":5001,"src":"172.30.90.2/32","protocol":89,"action":"unreachable"}
		]`),
		commandKey("ip", "-n", Namespace, "-json", "-details", "link", "show"): []byte(`[
			{"ifname":"lo","ifindex":1,"flags":["LOOPBACK","UP"]},
			{"ifname":"vpnkit-ns","ifindex":20,"link_index":10,"flags":["BROADCAST","UP"],"linkinfo":{"info_kind":"veth"}}
		]`),
		commandKey("ip", "-n", Namespace, "-json", "address", "show"): []byte(`[
			{"ifname":"lo","addr_info":[{"family":"inet","local":"127.0.0.1","prefixlen":8}]},
			{"ifname":"vpnkit-ns","addr_info":[{"family":"inet","local":"172.30.90.2","prefixlen":30}]}
		]`),
		commandKey("ip", "-n", Namespace, "-json", "-4", "route", "show", "table", "main"): []byte(`[
			{"dst":"default","gateway":"172.30.90.1","dev":"vpnkit-ns","table":"main","protocol":"boot"}
		]`),
		commandKey("ip", "netns", "exec", Namespace, "sysctl", "-n", "net.ipv6.conf.all.disable_ipv6"):     []byte("1\n"),
		commandKey("ip", "netns", "exec", Namespace, "sysctl", "-n", "net.ipv6.conf.default.disable_ipv6"): []byte("1\n"),
		commandKey("nft", "-j", "list", "tables"):                                                          []byte(`{"nftables":[{"metainfo":{"json_schema_version":1}},{"table":{"family":"inet","name":"vpnkit_native","handle":9}}]}`),
		commandKey("nft", "-j", "list", "table", NFTFamily, NFTTable):                                      nft,
	}
	filesystem := &inspectFS{
		stat: LinuxNamespaceStat{Device: 7, Inode: 99, UID: 0, FSType: unix.NSFS_MAGIC},
		files: map[string][]byte{
			"/proc/sys/net/ipv4/conf/vpnkit-host/route_localnet": []byte("1\n"),
			"/proc/sys/net/ipv4/ip_forward":                      []byte("1\n"),
		},
	}
	return outputs, filesystem
}

func observationFor(t *testing.T, inspection NetworkInspection, expected NetworkExpectation, kind EffectKind) EffectObservation {
	t.Helper()
	for n := 0; n < int(expected.EffectCount); n++ {
		if expected.Effects[n] == kind {
			return inspection.Effects[n]
		}
	}
	t.Fatalf("effect %d not present", kind)
	return EffectObservation{}
}

func TestLinuxNetworkInspectorVerifiesEveryExactEffectReadOnly(t *testing.T) {
	outputs, filesystem := exactInspectFixture(t)
	runner := &fixtureRunner{outputs: outputs}
	inspector := NewLinuxNetworkInspector(runner, filesystem)
	expected := fullExpectation(t, false)
	got, err := inspector.Inspect(context.Background(), expected)
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < int(expected.EffectCount); n++ {
		if got.Effects[n].State != ObservationExact {
			t.Errorf("effect %d (%d) = %s", n, expected.Effects[n], observationName(got.Effects[n].State))
		}
	}
	if identity := observationFor(t, got, expected, EffectGuestMoved).Identity; identity.Namespace.Inode != 99 || identity.Veth != (VethIdentity{HostIfIndex: 10, GuestIfIndex: 20}) {
		t.Fatalf("immutable identity not retained: %#v", identity)
	}
	for _, call := range runner.calls {
		if call == commandKey("iptables", "-w", "5", "-S", "FORWARD") {
			continue
		}
		lower := strings.ToLower(call)
		for _, forbidden := range []string{"\x00add\x00", "\x00del\x00", "\x00delete\x00", "\x00set\x00", "\x00-w\x00", "systemctl", "sh\x00", "bash\x00"} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("mutating or shell command used: %q", call)
			}
		}
	}
}

func TestLinuxNetworkInspectorReportsCascadedDeletionAndPartialInstall(t *testing.T) {
	outputs, filesystem := exactInspectFixture(t)
	filesystem.statErr = fs.ErrNotExist
	outputs[commandKey("iptables", "-w", "5", "-S", "FORWARD")] = []byte("-P FORWARD DROP\n")
	filesystem.files = map[string][]byte{"/proc/sys/net/ipv4/ip_forward": []byte("0\n")}
	outputs[commandKey("ip", "-json", "-details", "link", "show")] = []byte(`[{"ifname":"lo","ifindex":1,"flags":["UP"]},{"ifname":"enp5s0","ifindex":2,"flags":["UP"]}]`)
	outputs[commandKey("ip", "-json", "address", "show")] = []byte(`[{"ifname":"lo","addr_info":[]},{"ifname":"enp5s0","addr_info":[{"family":"inet","local":"192.0.2.10","prefixlen":24}]}]`)
	outputs[commandKey("ip", "-json", "-4", "route", "show", "table", "21194")] = []byte(`[]`)
	outputs[commandKey("ip", "-json", "-4", "rule", "show")] = []byte(`[]`)
	outputs[commandKey("nft", "-j", "list", "tables")] = []byte(`{"nftables":[]}`)
	got, err := NewLinuxNetworkInspector(&fixtureRunner{outputs: outputs}, filesystem).Inspect(context.Background(), fullExpectation(t, false))
	if err != nil {
		t.Fatal(err)
	}
	expected := fullExpectation(t, false)
	for n := 0; n < int(expected.EffectCount); n++ {
		if got.Effects[n].State != ObservationAbsent {
			t.Errorf("deleted effect %d = %s, want absent", expected.Effects[n], observationName(got.Effects[n].State))
		}
	}

	// A namespace marker that cannot be inspected does not prove guest effects absent.
	filesystem.statErr = errors.New("permission denied")
	got, err = NewLinuxNetworkInspector(&fixtureRunner{outputs: outputs}, filesystem).Inspect(context.Background(), expected)
	if err != nil {
		t.Fatal(err)
	}
	if observationFor(t, got, expected, EffectGuestAddress).State != ObservationUnverifiable {
		t.Fatal("inaccessible namespace incorrectly treated as absent")
	}
}

func TestLinuxNetworkInspectorReportsPartialInstallWithoutAssumingLaterEffects(t *testing.T) {
	outputs, filesystem := exactInspectFixture(t)
	outputs[commandKey("ip", "-json", "-details", "link", "show")] = []byte(`[
		{"ifname":"lo","ifindex":1,"flags":["UP"]},
		{"ifname":"enp5s0","ifindex":2,"flags":["UP"]},
		{"ifname":"vpnkit-host","ifindex":10,"link_index":20,"flags":[],"linkinfo":{"info_kind":"veth"}},
		{"ifname":"vpnkit-ns","ifindex":20,"link_index":10,"flags":[],"linkinfo":{"info_kind":"veth"}}
	]`)
	outputs[commandKey("ip", "-n", Namespace, "-json", "-details", "link", "show")] = []byte(`[{"ifname":"lo","ifindex":1,"flags":[]}]`)
	outputs[commandKey("ip", "-json", "address", "show")] = []byte(`[
		{"ifname":"lo","addr_info":[]},{"ifname":"enp5s0","addr_info":[{"family":"inet","local":"192.0.2.10","prefixlen":24}]},
		{"ifname":"vpnkit-host","addr_info":[]},{"ifname":"vpnkit-ns","addr_info":[]}
	]`)
	outputs[commandKey("ip", "-n", Namespace, "-json", "address", "show")] = []byte(`[{"ifname":"lo","addr_info":[]}]`)
	outputs[commandKey("ip", "-json", "-4", "route", "show", "table", "21194")] = []byte(`[]`)
	outputs[commandKey("ip", "-json", "-4", "rule", "show")] = []byte(`[]`)
	outputs[commandKey("ip", "-n", Namespace, "-json", "-4", "route", "show", "table", "main")] = []byte(`[]`)
	outputs[commandKey("ip", "netns", "exec", Namespace, "sysctl", "-n", "net.ipv6.conf.all.disable_ipv6")] = []byte("0\n")
	outputs[commandKey("ip", "netns", "exec", Namespace, "sysctl", "-n", "net.ipv6.conf.default.disable_ipv6")] = []byte("0\n")
	outputs[commandKey("nft", "-j", "list", "tables")] = []byte(`{"nftables":[]}`)
	filesystem.files["/proc/sys/net/ipv4/conf/vpnkit-host/route_localnet"] = []byte("0\n")
	filesystem.files["/proc/sys/net/ipv4/ip_forward"] = []byte("0\n")
	expected := fullExpectation(t, false)
	got, err := NewLinuxNetworkInspector(&fixtureRunner{outputs: outputs}, filesystem).Inspect(context.Background(), expected)
	if err != nil {
		t.Fatal(err)
	}
	if observationFor(t, got, expected, EffectNamespace).State != ObservationExact || observationFor(t, got, expected, EffectVeth).State != ObservationExact {
		t.Fatal("created namespace/veth were not recognized")
	}
	for _, kind := range []EffectKind{EffectGuestMoved, EffectHostAddress, EffectGuestAddress, EffectConnectedRoute, EffectEgressRule, EffectNFTTable, EffectHostUp} {
		if state := observationFor(t, got, expected, kind).State; state != ObservationAbsent {
			t.Errorf("later effect %d = %s, want absent", kind, observationName(state))
		}
	}
}

func TestLinuxNetworkInspectorExposesReplacementIdentities(t *testing.T) {
	outputs, filesystem := exactInspectFixture(t)
	expected := fullExpectation(t, false)
	first, err := NewLinuxNetworkInspector(&fixtureRunner{outputs: outputs}, filesystem).Inspect(context.Background(), expected)
	if err != nil {
		t.Fatal(err)
	}
	filesystem.stat.Inode = 100
	outputs[commandKey("ip", "-json", "-details", "link", "show")] = []byte(`[{"ifname":"lo","ifindex":1,"flags":["UP"]},{"ifname":"enp5s0","ifindex":2,"flags":["UP"]},{"ifname":"vpnkit-host","ifindex":30,"link_index":40,"flags":["UP"],"linkinfo":{"info_kind":"veth"}}]`)
	outputs[commandKey("ip", "-n", Namespace, "-json", "-details", "link", "show")] = []byte(`[{"ifname":"lo","ifindex":1,"flags":["UP"]},{"ifname":"vpnkit-ns","ifindex":40,"link_index":30,"flags":["UP"],"linkinfo":{"info_kind":"veth"}}]`)
	second, err := NewLinuxNetworkInspector(&fixtureRunner{outputs: outputs}, filesystem).Inspect(context.Background(), expected)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(observationFor(t, first, expected, EffectGuestMoved).Identity, observationFor(t, second, expected, EffectGuestMoved).Identity) {
		t.Fatal("same-name namespace/veth replacement was not visible to journal identity checks")
	}

	filesystem.stat.UID = 1000
	third, err := NewLinuxNetworkInspector(&fixtureRunner{outputs: outputs}, filesystem).Inspect(context.Background(), expected)
	if err != nil {
		t.Fatal(err)
	}
	if observationFor(t, third, expected, EffectNamespace).State != ObservationConflict {
		t.Fatal("non-root namespace marker accepted")
	}
}

func TestLinuxNetworkInspectorRejectsWrongRoutesRulesAndExtraNFT(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string][]byte)
		kind   EffectKind
	}{
		{name: "route source", kind: EffectConnectedRoute, mutate: func(o map[string][]byte) {
			o[commandKey("ip", "-json", "-4", "route", "show", "table", "21194")] = []byte(`[{"dst":"192.0.2.0/24","dev":"enp5s0","prefsrc":"192.0.2.11","scope":"link","table":21194,"protocol":89}]`)
		}},
		{name: "route protocol", kind: EffectDefaultUplinkRoute, mutate: func(o map[string][]byte) {
			o[commandKey("ip", "-json", "-4", "route", "show", "table", "21194")] = []byte(`[{"dst":"default","gateway":"192.0.2.1","dev":"enp5s0","table":21194,"protocol":88}]`)
		}},
		{name: "rule priority selector", kind: EffectEgressRule, mutate: func(o map[string][]byte) {
			o[commandKey("ip", "-json", "-4", "rule", "show")] = []byte(`[{"priority":4999,"src":"172.30.90.2/32","iif":"vpnkit-host","table":21194,"protocol":89}]`)
		}},
		{name: "nft extra rule", kind: EffectNFTTable, mutate: func(o map[string][]byte) {
			var doc map[string]any
			if err := json.Unmarshal(o[commandKey("nft", "-j", "list", "table", NFTFamily, NFTTable)], &doc); err != nil {
				panic(err)
			}
			rows := doc["nftables"].([]any)
			doc["nftables"] = append(rows, map[string]any{"rule": map[string]any{"family": NFTFamily, "table": NFTTable, "chain": "forward", "expr": []any{map[string]any{"accept": nil}}}})
			o[commandKey("nft", "-j", "list", "table", NFTFamily, NFTTable)], _ = json.Marshal(doc)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			outputs, filesystem := exactInspectFixture(t)
			tc.mutate(outputs)
			expected := fullExpectation(t, false)
			got, err := NewLinuxNetworkInspector(&fixtureRunner{outputs: outputs}, filesystem).Inspect(context.Background(), expected)
			if err != nil {
				t.Fatal(err)
			}
			if state := observationFor(t, got, expected, tc.kind).State; state != ObservationConflict {
				t.Fatalf("state = %s", observationName(state))
			}
		})
	}
}

func TestLinuxNetworkInspectorRejectsMalformedSuccessfulJSON(t *testing.T) {
	outputs, filesystem := exactInspectFixture(t)
	outputs[commandKey("nft", "-j", "list", "table", NFTFamily, NFTTable)] = []byte(`{"nftables":[`)
	if _, err := NewLinuxNetworkInspector(&fixtureRunner{outputs: outputs}, filesystem).Inspect(context.Background(), fullExpectation(t, false)); err == nil {
		t.Fatal("invalid nft JSON accepted")
	}
}

func TestLinuxNetworkInspectorSnapshotDelegatesToCompleteProbe(t *testing.T) {
	runner := &fixtureRunner{outputs: validProbeFixture()}
	got, err := NewLinuxNetworkInspector(runner, &inspectFS{}).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Interfaces) == 0 || len(got.Routes) == 0 || len(got.Rules) == 0 {
		t.Fatalf("incomplete snapshot: %#v", got)
	}
	if len(runner.calls) != len(probeCommands) {
		t.Fatalf("Snapshot made %d calls, want %d", len(runner.calls), len(probeCommands))
	}
}
