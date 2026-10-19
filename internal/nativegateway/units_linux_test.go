//go:build linux

package nativegateway

import (
	"reflect"
	"strings"
	"testing"
)

func unitDirective(t *testing.T, unit, key string) string {
	t.Helper()
	prefix := key + "="
	for _, line := range strings.Split(unit, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	t.Fatalf("unit is missing %s", key)
	return ""
}

func unitHasLine(unit, want string) bool {
	for _, line := range strings.Split(unit, "\n") {
		if line == want {
			return true
		}
	}
	return false
}

func requireUnitLine(t *testing.T, unit, want string) {
	t.Helper()
	if !unitHasLine(unit, want) {
		t.Fatalf("unit is missing line %q\n%s", want, unit)
	}
}

func assertNoInstallSection(t *testing.T, unit string) {
	t.Helper()
	if unitHasLine(unit, "[Install]") {
		t.Fatal("rendered unit unexpectedly has an [Install] section")
	}
	for _, line := range strings.Split(unit, "\n") {
		if strings.HasPrefix(line, "WantedBy=") || strings.HasPrefix(line, "RequiredBy=") || strings.HasPrefix(line, "Alias=") {
			t.Fatalf("rendered unit has install side effect %q", line)
		}
	}
}

func TestUnitFilesAreFixedPureArtifacts(t *testing.T) {
	got := UnitFiles()
	if len(got) != 2 {
		t.Fatalf("got %d unit files, want 2", len(got))
	}
	if _, ok := got[NetworkUnit]; !ok {
		t.Fatalf("missing %s", NetworkUnit)
	}
	if _, ok := got[GatewayUnit]; !ok {
		t.Fatalf("missing %s", GatewayUnit)
	}

	first := UnitFiles()
	first[NetworkUnit] = "caller mutation"
	second := UnitFiles()
	if second[NetworkUnit] == "caller mutation" {
		t.Fatal("UnitFiles shares mutable output between calls")
	}
	if !reflect.DeepEqual(UnitFiles(), second) {
		t.Fatal("UnitFiles did not return fixed contents")
	}

	all := strings.ToLower(second[NetworkUnit] + "\n" + second[GatewayUnit])
	for _, forbidden := range []string{
		"docker",
		"systemctl",
		"enable",
		"--now",
		"[install]",
		"wantedby=",
		"requiredby=",
		"alias=",
		"%h",
		"$home",
		"/home/",
		"/root/",
	} {
		if strings.Contains(all, forbidden) {
			t.Fatalf("rendered units contain forbidden %q", forbidden)
		}
	}
	for _, unit := range []string{second[NetworkUnit], second[GatewayUnit]} {
		if strings.Contains(unit, "$") || strings.Contains(unit, "%") {
			t.Fatalf("rendered unit contains interpolation syntax: %q", unit)
		}
		for _, line := range strings.Split(unit, "\n") {
			if strings.HasPrefix(line, "Environment=") || strings.HasPrefix(line, "EnvironmentFile=") || strings.HasPrefix(line, "PassEnvironment=") {
				t.Fatalf("rendered unit accepts caller environment through %q", line)
			}
		}
	}
}

func TestNetworkUnitIsRootOrderedOneshot(t *testing.T) {
	unit := UnitFiles()[NetworkUnit]
	assertNoInstallSection(t, unit)
	for key, want := range map[string]string{
		"Type":            "oneshot",
		"User":            "root",
		"Group":           "root",
		"RemainAfterExit": "yes",
		"After":           "NetworkManager.service network-online.target",
		"Before":          GatewayUnit,
		"ExecStart":       "/usr/libexec/local-vpn-kde/local-vpn-kde.bin native-network --action start",
		"ExecStop":        "/usr/libexec/local-vpn-kde/local-vpn-kde.bin native-network --action stop",
	} {
		if got := unitDirective(t, unit, key); got != want {
			t.Errorf("%s=%q, want %q", key, got, want)
		}
	}
	if strings.Contains(unit, "Restart=") {
		t.Error("network setup unit should not become a restart policy")
	}
}

func TestGatewayUnitUsesPreparedNamespaceAndBoundedPrivileges(t *testing.T) {
	unit := UnitFiles()[GatewayUnit]
	assertNoInstallSection(t, unit)

	for key, want := range map[string]string{
		"Requires":              NetworkUnit,
		"After":                 NetworkUnit,
		"BindsTo":               NetworkUnit,
		"NetworkNamespacePath":  "/run/netns/" + Namespace,
		"User":                  ServiceUser,
		"Group":                 ServiceUser,
		"Type":                  "notify",
		"NotifyAccess":          "main",
		"TimeoutStartSec":       "90s",
		"ExecStart":             "/usr/libexec/local-vpn-kde/local-vpn-kde.bin native-serve",
		"ExecStartPre":          "+/usr/libexec/local-vpn-kde/local-vpn-kde.bin native-package --action stage-runtime",
		"Restart":               "on-failure",
		"RestartSec":            "5s",
		"TimeoutStopSec":        "30s",
		"StateDirectory":        "local-vpn-kde",
		"StateDirectoryMode":    "0700",
		"RuntimeDirectory":      "local-vpn-kde",
		"RuntimeDirectoryMode":  "0700",
		"WorkingDirectory":      StateDir,
		"UMask":                 "0077",
		"CapabilityBoundingSet": "CAP_NET_ADMIN CAP_NET_RAW",
		"AmbientCapabilities":   "CAP_NET_ADMIN CAP_NET_RAW",
		"NoNewPrivileges":       "yes",
		"SetLoginEnvironment":   "no",
	} {
		if got := unitDirective(t, unit, key); got != want {
			t.Errorf("%s=%q, want %q", key, got, want)
		}
	}
	for _, line := range []string{
		"DevicePolicy=closed",
		"DeviceAllow=/dev/net/tun rw",
		"PrivateTmp=yes",
		"ProtectSystem=strict",
		"ProtectHome=yes",
		"ProtectKernelTunables=yes",
		"ProtectKernelModules=yes",
		"ProtectKernelLogs=yes",
		"ProtectControlGroups=yes",
		"ReadOnlyPaths=" + ConfigDir,
		"ReadWritePaths=" + StateDir + " /run/local-vpn-kde /var/lib/local-vpn-kde-runtime",
	} {
		requireUnitLine(t, unit, line)
	}
	for _, line := range strings.Split(unit, "\n") {
		if strings.HasPrefix(line, "PrivateNetwork=") {
			t.Fatalf("gateway must use the prepared namespace, not %q", line)
		}
	}
}
