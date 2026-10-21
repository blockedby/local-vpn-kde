//go:build linux

package nativegateway

import (
	"strings"
	"testing"
)

func TestForwardOwnershipRequiresExactUniqueRule(t *testing.T) {
	for _, returning := range []bool{false, true} {
		rule := strings.Join(append([]string{"-A", "FORWARD"}, forwardCompatibilityRule("eth0", returning)...), " ") + "\n"
		for _, tc := range []struct {
			body string
			want ObservationState
		}{
			{"-P FORWARD DROP\n-A FORWARD -j DOCKER-USER\n", ObservationAbsent},
			{"-P FORWARD DROP\n" + rule + "-A FORWARD -j DOCKER-USER\n", ObservationExact},
			{rule + rule, ObservationConflict},
			{strings.Replace(rule, "/32", "/24", 1), ObservationConflict},
			{strings.Replace(rule, "eth0", "eth1", 1), ObservationConflict},
			{strings.Replace(rule, "-j ACCEPT", "-j DROP", 1), ObservationConflict},
		} {
			if got := observeForwardCompatibility(tc.body, "eth0", returning); got != tc.want {
				t.Fatalf("%q: got %v want %v", tc.body, got, tc.want)
			}
		}
	}
}
