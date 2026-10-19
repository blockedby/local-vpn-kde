//go:build linux

package nativegateway

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type fixtureRunner struct {
	outputs map[string][]byte
	calls   []string
	errKey  string
}

func commandKey(program string, args ...string) string {
	return program + "\x00" + strings.Join(args, "\x00")
}

func (runner *fixtureRunner) Run(_ context.Context, program string, args ...string) ([]byte, error) {
	key := commandKey(program, args...)
	runner.calls = append(runner.calls, key)
	if key == runner.errKey {
		return nil, errors.New("fixture command failed")
	}
	output, exists := runner.outputs[key]
	if !exists {
		return nil, errors.New("unexpected command")
	}
	return output, nil
}

func validProbeFixture() map[string][]byte {
	return map[string][]byte{
		commandKey("ip", "-json", "link", "show"): []byte(`[
  {"ifindex":1,"ifname":"lo","flags":["LOOPBACK","UP"]},
  {"ifindex":2,"ifname":"enp5s0","flags":["BROADCAST","UP"],"link_type":"ether"},
  {"ifindex":3,"ifname":"tun0","flags":["POINTOPOINT","UP"],"link_type":"none"}
]`),
		commandKey("ip", "-json", "address", "show"): []byte(`[
  {"ifindex":1,"ifname":"lo","addr_info":[{"family":"inet","local":"127.0.0.1","prefixlen":8,"scope":"host"},{"family":"inet6","local":"::1","prefixlen":128,"scope":"host"}]},
  {"ifindex":2,"ifname":"enp5s0","addr_info":[{"family":"inet","local":"192.0.2.10","prefixlen":24,"broadcast":"192.0.2.255","scope":"global","dynamic":true},{"family":"inet6","local":"2001:db8::10","prefixlen":64,"scope":"global"}]},
  {"ifindex":3,"ifname":"tun0","addr_info":[{"family":"inet","local":"10.8.0.2","prefixlen":24,"scope":"global"}]}
]`),
		commandKey("ip", "-json", "-4", "route", "show", "table", "all"): []byte(`[
  {"dst":"default","gateway":"192.0.2.1","dev":"enp5s0","protocol":"dhcp","prefsrc":"192.0.2.10","metric":100,"table":"main"},
  {"dst":"192.0.2.0/24","dev":"enp5s0","protocol":"kernel","scope":"link","prefsrc":"192.0.2.10","table":"main"},
  {"type":"local","dst":"192.0.2.10/32","dev":"enp5s0","protocol":"kernel","scope":"host","table":"local"},
  {"dst":"10.8.0.0/24","dev":"tun0","protocol":"kernel","scope":"link","prefsrc":"10.8.0.2","table":254}
]`),
		commandKey("ip", "-json", "-4", "rule", "show"): []byte(`[
  {"priority":0,"src":"all","table":"local"},
  {"priority":32766,"src":"all","table":"main"},
  {"priority":32767,"src":"all","table":"default"}
]`),
		commandKey("ip", "netns", "list"):                                                                 []byte("build-lab (id: 2)\n"),
		commandKey("nft", "-j", "list", "tables"):                                                         []byte(`{"nftables":[{"metainfo":{"version":"1.0.9","json_schema_version":1}},{"table":{"family":"inet","name":"filter","handle":1}},{"table":{"family":"ip","name":"nat","handle":2}}]}`),
		commandKey("sysctl", "-n", "net.ipv4.ip_forward"):                                                 []byte("1\n"),
		commandKey("nmcli", "-t", "--escape", "yes", "--fields", "DEVICE,TYPE,STATE", "device", "status"): []byte("enp5s0:ethernet:connected\ntun0:tun:connected (externally)\nlo:loopback:connected (externally)\n"),
	}
}

func cloneFixture(source map[string][]byte) map[string][]byte {
	result := make(map[string][]byte, len(source))
	for key, value := range source {
		result[key] = append([]byte(nil), value...)
	}
	return result
}

func TestProbeNetworkBuildsCompleteReadOnlySnapshot(t *testing.T) {
	runner := &fixtureRunner{outputs: validProbeFixture()}
	uplink, snapshot, err := ProbeNetwork(context.Background(), runner)
	if err != nil {
		t.Fatal(err)
	}
	wantUplink := Uplink{Interface: "enp5s0", AddressCIDR: "192.0.2.10/24", Gateway: "192.0.2.1"}
	if !reflect.DeepEqual(uplink, wantUplink) {
		t.Fatalf("uplink = %#v, want %#v", uplink, wantUplink)
	}
	if len(snapshot.Interfaces) != 3 || len(snapshot.Routes) != 4 || len(snapshot.Rules) != 3 || len(snapshot.NFTables) != 2 {
		t.Fatalf("incomplete snapshot: %#v", snapshot)
	}
	if !snapshot.IPv4Forwarding || !reflect.DeepEqual(snapshot.Namespaces, []string{"build-lab"}) {
		t.Fatalf("unexpected scalar snapshot facts: %#v", snapshot)
	}
	if len(snapshot.Interfaces[1].Addresses) != 2 || snapshot.Interfaces[1].Addresses[0] != "192.0.2.10/24" {
		t.Fatalf("interface addresses not preserved: %#v", snapshot.Interfaces[1])
	}
	if snapshot.Routes[0].Table != 254 || snapshot.Routes[0].Protocol != 16 || snapshot.Rules[0].Table != 255 {
		t.Fatalf("named route metadata not normalized: routes=%#v rules=%#v", snapshot.Routes, snapshot.Rules)
	}
	if _, err := PlanNetwork(uplink, snapshot); err != nil {
		t.Fatalf("probed fixture should be accepted by planner: %v", err)
	}

	if len(runner.calls) != len(probeCommands) {
		t.Fatalf("got %d calls, want %d", len(runner.calls), len(probeCommands))
	}
	for index, command := range probeCommands {
		want := commandKey(command.program, command.args...)
		if runner.calls[index] != want {
			t.Fatalf("call %d = %q, want %q", index, runner.calls[index], want)
		}
		joined := strings.ToLower(runner.calls[index])
		if strings.Contains(joined, "docker") || strings.Contains(joined, "podman") || strings.Contains(joined, " add\x00") || strings.Contains(joined, " delete\x00") {
			t.Fatalf("forbidden probe command: %q", runner.calls[index])
		}
	}
}

func TestProbeNetworkRejectsUnprovenAndAmbiguousDefaults(t *testing.T) {
	routeKey := commandKey("ip", "-json", "-4", "route", "show", "table", "all")
	linkKey := commandKey("ip", "-json", "link", "show")
	addressKey := commandKey("ip", "-json", "address", "show")
	nmKey := commandKey("nmcli", "-t", "--escape", "yes", "--fields", "DEVICE,TYPE,STATE", "device", "status")
	tests := []struct {
		name   string
		change func(map[string][]byte)
	}{
		{name: "VPN default", change: func(f map[string][]byte) {
			f[routeKey] = []byte(`[{"dst":"default","gateway":"192.0.2.1","dev":"enp5s0","table":"main"},{"dst":"default","dev":"tun0","table":100}]`)
		}},
		{name: "no physical default", change: func(f map[string][]byte) {
			f[routeKey] = []byte(`[{"dst":"192.0.2.0/24","dev":"enp5s0","protocol":"kernel"}]`)
		}},
		{name: "name alone is not physical proof", change: func(f map[string][]byte) {
			f[nmKey] = []byte("enp5s0:bridge:connected\ntun0:tun:connected\n")
		}},
		{name: "two physical defaults", change: func(f map[string][]byte) {
			f[linkKey] = []byte(`[{"ifname":"lo"},{"ifname":"enp5s0"},{"ifname":"wlp2s0"},{"ifname":"tun0"}]`)
			f[addressKey] = []byte(`[{"ifname":"lo","addr_info":[{"family":"inet","local":"127.0.0.1","prefixlen":8}]},{"ifname":"enp5s0","addr_info":[{"family":"inet","local":"192.0.2.10","prefixlen":24}]},{"ifname":"wlp2s0","addr_info":[{"family":"inet","local":"198.51.100.10","prefixlen":24}]},{"ifname":"tun0","addr_info":[{"family":"inet","local":"10.8.0.2","prefixlen":24}]}]`)
			f[routeKey] = []byte(`[{"dst":"default","gateway":"192.0.2.1","dev":"enp5s0"},{"dst":"default","gateway":"198.51.100.1","dev":"wlp2s0"}]`)
			f[nmKey] = []byte("enp5s0:ethernet:connected\nwlp2s0:wifi:connected\ntun0:tun:connected\n")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := cloneFixture(validProbeFixture())
			test.change(fixture)
			if uplink, snapshot, err := ProbeNetwork(context.Background(), &fixtureRunner{outputs: fixture}); err == nil || uplink != (Uplink{}) || !reflect.DeepEqual(snapshot, Snapshot{}) {
				t.Fatalf("ProbeNetwork() = %#v, %#v, %v; want fail-closed result", uplink, snapshot, err)
			}
		})
	}
}

func TestProbeNetworkRejectsUnknownEarlyGuestRule(t *testing.T) {
	fixture := validProbeFixture()
	fixture[commandKey("ip", "-json", "-4", "rule", "show")] = []byte(`[
  {"priority":0,"src":"all","table":"local"},
  {"priority":1000,"src":"172.30.89.0/24","table":100},
  {"priority":32766,"src":"all","table":"main"}
]`)
	if _, _, err := ProbeNetwork(context.Background(), &fixtureRunner{outputs: fixture}); err == nil || !strings.Contains(err.Error(), "may affect native guest routing") {
		t.Fatalf("expected early-rule rejection, got %v", err)
	}
}

func TestProbeNetworkAllowsEarlyRuleScopedAwayFromGuest(t *testing.T) {
	fixture := validProbeFixture()
	fixture[commandKey("ip", "-json", "-4", "rule", "show")] = []byte(`[
  {"priority":0,"src":"all","table":"local"},
  {"priority":1000,"src":"10.0.0.0/8","table":100},
  {"priority":32766,"src":"all","table":"main"}
]`)
	if _, _, err := ProbeNetwork(context.Background(), &fixtureRunner{outputs: fixture}); err != nil {
		t.Fatalf("unrelated source rule rejected: %v", err)
	}
}

func TestProbeNetworkRejectsMalformedTruncatedAndOversizedOutput(t *testing.T) {
	linkKey := commandKey("ip", "-json", "link", "show")
	routeKey := commandKey("ip", "-json", "-4", "route", "show", "table", "all")
	tests := []struct {
		name   string
		key    string
		output []byte
	}{
		{name: "truncated JSON", key: routeKey, output: []byte(`[{"dst":"default"`)},
		{name: "trailing JSON", key: routeKey, output: []byte(`[] []`)},
		{name: "wrong JSON shape", key: routeKey, output: []byte(`null`)},
		{name: "oversized", key: linkKey, output: make([]byte, ProbeMaxOutputBytes+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := cloneFixture(validProbeFixture())
			fixture[test.key] = test.output
			if _, _, err := ProbeNetwork(context.Background(), &fixtureRunner{outputs: fixture}); err == nil {
				t.Fatal("expected malformed or oversized output rejection")
			}
		})
	}
}

func TestProbeNetworkPropagatesReadFailureAndCancellation(t *testing.T) {
	fixture := validProbeFixture()
	failure := commandKey("nft", "-j", "list", "tables")
	if _, _, err := ProbeNetwork(context.Background(), &fixtureRunner{outputs: fixture, errKey: failure}); err == nil {
		t.Fatal("expected command failure")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner := &fixtureRunner{outputs: fixture}
	if _, _, err := ProbeNetwork(ctx, runner); !errors.Is(err, context.Canceled) || len(runner.calls) != 0 {
		t.Fatalf("canceled ProbeNetwork() error/calls = %v/%d", err, len(runner.calls))
	}
}

func TestProbedKernelOverlapIsRejectedByPlannerWithoutDocker(t *testing.T) {
	fixture := validProbeFixture()
	fixture[commandKey("ip", "-json", "link", "show")] = []byte(`[{"ifname":"lo"},{"ifname":"enp5s0"},{"ifname":"tun0"},{"ifname":"docker0"}]`)
	fixture[commandKey("ip", "-json", "address", "show")] = []byte(`[
  {"ifname":"lo","addr_info":[{"family":"inet","local":"127.0.0.1","prefixlen":8}]},
  {"ifname":"enp5s0","addr_info":[{"family":"inet","local":"192.0.2.10","prefixlen":24}]},
  {"ifname":"tun0","addr_info":[{"family":"inet","local":"10.8.0.2","prefixlen":24}]},
  {"ifname":"docker0","addr_info":[{"family":"inet","local":"172.30.89.1","prefixlen":24}]}
]`)
	fixture[commandKey("ip", "-json", "-4", "route", "show", "table", "all")] = []byte(`[
  {"dst":"default","gateway":"192.0.2.1","dev":"enp5s0","protocol":"dhcp","prefsrc":"192.0.2.10"},
  {"dst":"172.30.89.0/24","dev":"docker0","protocol":"kernel","scope":"link","prefsrc":"172.30.89.1"}
]`)
	uplink, snapshot, err := ProbeNetwork(context.Background(), &fixtureRunner{outputs: fixture})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PlanNetwork(uplink, snapshot); err == nil || !strings.Contains(err.Error(), "overlaps native transit") {
		t.Fatalf("planner did not reject kernel-visible overlap: %v", err)
	}
}
