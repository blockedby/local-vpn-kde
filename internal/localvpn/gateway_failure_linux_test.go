package localvpn

import (
	"errors"
	"strings"
	"testing"
)

func TestGatewayFailureDiagnosticsRedactAndClassify(t *testing.T) {
	for _, tc := range []struct{ line, reason string }{
		{"FATAL initialize rule-set[geoip-ru]: download: lookup private.example: no such host", "rule-download-dns"},
		{"FATAL initialize rule-set[x]: download: context deadline exceeded https://private.example/token", "rule-download-timeout"},
		{"FATAL initialize rule-set[x]: download: connection refused https://private.example/token", "rule-download-refused"},
		{"FATAL initialize rule-set[x]: download: network is unreachable", "rule-download-network"},
		{"FATAL initialize rule-set[x]: download: bad certificate", "rule-download-failed"},
		{"INFO download rule-set[x] https://private.example/token", "exited"},
		{"FATAL unrelated private.example token", "exited"},
	} {
		t.Run(tc.reason+tc.line[:5], func(t *testing.T) {
			d := &gatewayChildDiagnostics{}
			_, _ = d.Write([]byte(tc.line[:10]))
			_, _ = d.Write([]byte(tc.line[10:] + "\n"))
			if got := d.reason(); got != tc.reason {
				t.Fatalf("reason = %s, want %s", got, tc.reason)
			}
			message := NativeGatewayFailureMessage(gatewayFailure("sing-box", d.reason(), errors.New(tc.line)))
			if !strings.Contains(message, tc.reason) || strings.Contains(message, "private.example") || strings.Contains(message, "token") {
				t.Fatal(message)
			}
		})
	}
	if message := NativeGatewayFailureMessage(errors.New("https://private.example/token")); strings.Contains(message, "private") {
		t.Fatal(message)
	}
}
