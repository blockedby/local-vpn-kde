//go:build linux

package nativegateway

import "testing"

func TestProbeCanonicalizesIPRouteHostAddresses(t *testing.T) {
	data := []byte(`[{"type":"local","dst":"127.0.0.1","dev":"lo","table":"local","protocol":"kernel","scope":"host","prefsrc":"127.0.0.1"},{"type":"broadcast","dst":"192.0.2.255","dev":"eth0","table":"local","protocol":"kernel","scope":"link"}]`)
	routes, _, err := parseRoutes(data, []InterfaceState{{Name: "lo"}, {Name: "eth0"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 2 || routes[0].Destination != "127.0.0.1/32" || routes[1].Destination != "192.0.2.255/32" {
		t.Fatalf("unexpected routes: %#v", routes)
	}
	for _, destination := range []string{"::1", "not-an-address", "127.0.0.1/33"} {
		_, _, err := parseRoutes([]byte(`[{"dst":"`+destination+`","dev":"lo"}]`), []InterfaceState{{Name: "lo"}})
		if err == nil {
			t.Fatalf("invalid IPv4 destination accepted: %s", destination)
		}
	}
}
