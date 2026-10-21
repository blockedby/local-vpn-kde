package main

import (
	"context"
	"testing"
)

func TestNativeInitialEndpointMatchesIPv4Namespace(t *testing.T) {
	for _, server := range []string{"2001:db8::1", "127.0.0.1", "0.0.0.0", "224.0.0.1"} {
		if _, err := prepareNativeInitialOutbound(context.Background(), map[string]any{"type": "vless", "server": server}); err == nil {
			t.Fatal("unsupported endpoint accepted")
		}
	}
	out, err := prepareNativeInitialOutbound(context.Background(), map[string]any{"type": "vless", "server": "192.0.2.1"})
	if err != nil || out["server"] != "192.0.2.1" {
		t.Fatal("IPv4 endpoint rejected")
	}
}
