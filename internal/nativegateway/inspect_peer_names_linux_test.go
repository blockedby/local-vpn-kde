//go:build linux

package nativegateway

import (
	"context"
	"testing"
)

type linkFixture []byte

func (f linkFixture) Run(context.Context, string, ...string) ([]byte, error) { return f, nil }

func TestInspectorResolvesSameNamespacePeerNames(t *testing.T) {
	runner := linkFixture(`[{"ifindex":3,"ifname":"vpnkit-host","link":"vpnkit-ns","flags":[],"linkinfo":{"info_kind":"veth"}},{"ifindex":4,"ifname":"vpnkit-ns","link":"vpnkit-host","flags":[],"linkinfo":{"info_kind":"veth"}}]`)
	links, ok, err := NewLinuxNetworkInspector(runner).readLinks(context.Background(), "ip", "-json", "-details", "link", "show")
	if err != nil || !ok {
		t.Fatalf("parse links: ok=%v err=%v", ok, err)
	}
	if links[HostInterface].PeerIndex != 4 || links[GuestInterface].PeerIndex != 3 {
		t.Fatalf("unresolved peers: %#v", links)
	}
}

func TestInspectorRejectsUnresolvedPeerName(t *testing.T) {
	runner := linkFixture(`[{"ifindex":3,"ifname":"vpnkit-host","link":"missing","flags":[],"linkinfo":{"info_kind":"veth"}}]`)
	_, _, err := NewLinuxNetworkInspector(runner).readLinks(context.Background(), "ip", "-json", "-details", "link", "show")
	if err == nil {
		t.Fatal("unresolved peer accepted")
	}
}
