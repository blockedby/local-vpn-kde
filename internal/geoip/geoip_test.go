package geoip

import (
	"net/netip"
	"testing"
)

func TestLookup(t *testing.T) {
	for _, tt := range []struct{ address, country string }{
		{"1.1.1.1", "AU"}, {"2001:200::1", "JP"}, {"10.0.0.1", ""}, {"127.0.0.1", ""}, {"192.0.2.1", ""}, {"198.18.0.1", ""}, {"240.0.0.1", ""}, {"::1", ""}, {"fc00::1", ""}, {"2001:db8::1", ""},
	} {
		t.Run(tt.address, func(t *testing.T) {
			got, ok := Lookup(netip.MustParseAddr(tt.address))
			if got != tt.country || ok != (tt.country != "") {
				t.Fatalf("got %q, %v; want %q", got, ok, tt.country)
			}
		})
	}
}
