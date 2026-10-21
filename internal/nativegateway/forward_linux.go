//go:build linux

package nativegateway

import "strings"

// Docker's iptables FORWARD policy can remain DROP after its VPN stops. Only
// the fixed native transit flow is admitted here; the native nft table still
// applies its source/interface and established-return checks independently.
// Rules are journaled separately, never flush chains or change host policies.
func forwardCompatibilityRule(uplink string, returning bool) []string {
	if returning {
		return []string{"-d", GuestAddress + "/32", "-i", uplink, "-o", HostInterface, "-m", "comment", "--comment", "vpnkit-native-return", "-j", "ACCEPT"}
	}
	return []string{"-s", GuestAddress + "/32", "-i", HostInterface, "-o", uplink, "-m", "comment", "--comment", "vpnkit-native-egress", "-j", "ACCEPT"}
}
func forwardCompatibilityCommand(uplink string, returning bool, operation string) Command {
	args := []string{"-w", "5", operation, "FORWARD"}
	if operation == "-I" {
		args = append(args, "1")
	}
	return Command{Program: "iptables", Args: append(args, forwardCompatibilityRule(uplink, returning)...)}
}
func observeForwardCompatibility(output, uplink string, returning bool) ObservationState {
	tag := "vpnkit-native-egress"
	if returning {
		tag = "vpnkit-native-return"
	}
	expected := strings.Join(append([]string{"-A", "FORWARD"}, forwardCompatibilityRule(uplink, returning)...), " ")
	count := 0
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(line, tag) {
			continue
		}
		count++
		// iptables-save quotes comment strings on some versions.
		fields := strings.Fields(line)
		for i := range fields {
			if fields[i] == "\""+tag+"\"" {
				fields[i] = tag
			}
		}
		if strings.Join(fields, " ") != expected {
			return ObservationConflict
		}
	}
	if count == 0 {
		return ObservationAbsent
	}
	if count != 1 {
		return ObservationConflict
	}
	return ObservationExact
}
