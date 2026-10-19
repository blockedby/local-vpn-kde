//go:build linux

package nativegateway

import (
	"context"
	"strconv"
	"testing"
)

func TestInspectorNormalizesElidedBootProtocol(t *testing.T) {
	fixture := linkFixture(`[{"dst":"default","gateway":"172.30.89.1","dev":"vpnkit-ns","flags":[]}]`)
	routes, ok, err := NewLinuxNetworkInspector(fixture).readInspectRoutes(context.Background(), "ip", "-n", Namespace, "-json", "-4", "route", "show", "table", "main")
	if err != nil || !ok || len(routes) != 1 {
		t.Fatalf("parse default route ok=%v err=%v", ok, err)
	}
	if routes[0].Protocol != 3 {
		t.Fatalf("elided boot protocol became %d", routes[0].Protocol)
	}
}

func TestInspectorUsesQueriedTableWhenRouteOmitsTable(t *testing.T) {
	fixture := linkFixture(`[{"dst":"192.0.2.0/24","dev":"lab-uplink","protocol":"89","scope":"link","prefsrc":"192.0.2.10","flags":[]}]`)
	inspector := NewLinuxNetworkInspector(fixture)
	for _, tc := range []struct {
		table string
		want  int
	}{{strconv.Itoa(RoutingTable), RoutingTable}, {"main", 254}} {
		routes, ok, err := inspector.readInspectRoutes(context.Background(), "ip", "-json", "-4", "route", "show", "table", tc.table)
		if err != nil || !ok || len(routes) != 1 {
			t.Fatalf("parse routes ok=%v err=%v", ok, err)
		}
		if routes[0].Table != tc.want {
			t.Fatalf("table=%d want=%d", routes[0].Table, tc.want)
		}
	}
}
