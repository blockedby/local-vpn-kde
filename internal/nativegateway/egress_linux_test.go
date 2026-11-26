package nativegateway

import (
	"context"
	"strings"
	"testing"
)

func TestEgressMatchesCurrentPhysicalLease(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		missing, stale, rule bool
	}{{"exact", false, false, true}, {"missing routes", true, false, true}, {"renewed gateway", false, true, true}, {"missing rules", false, false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := validProbeFixture()
			key := commandKey("ip", "-json", "-4", "route", "show", "table", "all")
			routes := strings.TrimSuffix(string(fixture[key]), "]")
			if tc.stale {
				routes = strings.Replace(routes, "192.0.2.1\"", "192.0.2.9\"", 1)
			}
			if !tc.missing {
				routes += `,{"dst":"default","dev":"enp5s0","gateway":"192.0.2.1","table":21194,"protocol":89},{"dst":"192.0.2.0/24","scope":"link","prefsrc":"192.0.2.10","dev":"enp5s0","table":21194,"protocol":89}`
			}
			fixture[key] = []byte(routes + "]")
			if tc.rule {
				fixture[commandKey("ip", "-json", "-4", "rule", "show")] = []byte(`[{"priority":900,"src":"172.30.90.2","iif":"vpnkit-host","table":21194,"protocol":89},{"priority":901,"src":"172.30.90.2","action":"unreachable","protocol":89}]`)
			}
			ready, err := NewLinuxNetworkInspector(&fixtureRunner{outputs: fixture}).EgressReady(context.Background())
			if err != nil || ready != (!tc.missing && !tc.stale && tc.rule) {
				t.Fatalf("ready=%t err=%v", ready, err)
			}
		})
	}
}
