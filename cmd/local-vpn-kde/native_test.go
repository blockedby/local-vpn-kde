//go:build linux

package main

import (
	"strings"
	"testing"
)

func TestNativeNetworkRejectsUntrustedArguments(t *testing.T) {
	for _, args := range [][]string{
		{"native-network", "--repo=/tmp"},
		{"native-network", "--action=arbitrary"},
		{"native-network", "start", "sh"},
		{"native-namespace-init", "--config=/tmp"},
	} {
		if err := run(args); err == nil {
			t.Fatalf("accepted untrusted arguments: %v", args)
		}
	}
}

func TestNativeDesktopCLIRequiresAuthorization(t *testing.T) {
	for _, action := range []string{"backend-start", "start", "disconnect", "stop"} {
		err := run([]string{"native-desktop", "--repo", t.TempDir(), "--secrets-dir", "secrets/vpnkit-local", "--action", action})
		if err == nil || !strings.Contains(err.Error(), "--yes") {
			t.Fatalf("mutation %s was not rejected at authorization: %v", action, err)
		}
	}
}

func TestNativeBridgeCLIRejectsAdapterOverride(t *testing.T) {
	err := run([]string{"bridge", "--runtime", "native", "--lifecycle-executable", "/tmp/foreign-adapter"})
	if err == nil || !strings.Contains(err.Error(), "adapter overrides") {
		t.Fatalf("native adapter override accepted: %v", err)
	}
}

func TestNativeServeRejectsArguments(t *testing.T) {
	for _, arg := range []string{"--repo=/tmp", "--config=/tmp/config", "--action=start", "unexpected"} {
		if err := run([]string{"native-serve", arg}); err == nil || !strings.Contains(err.Error(), "takes no arguments") {
			t.Fatalf("argument %q was not rejected: %v", arg, err)
		}
	}
}
