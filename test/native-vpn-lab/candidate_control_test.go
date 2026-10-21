package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestCandidateControlConfigPreservesEndpointSecurityMetadataWithoutTUN(t *testing.T) {
	tlsMetadata := map[string]any{
		"enabled":     true,
		"server_name": "public-sni.example.test",
		"utls":        map[string]any{"enabled": true, "fingerprint": "chrome"},
		"reality":     map[string]any{"enabled": true, "public_key": "public-key", "short_id": "abcd"},
	}
	transportMetadata := map[string]any{
		"type": "ws", "path": "/public-path",
		"headers": map[string]any{"Host": "public-ws.example.test"},
	}
	prepared := map[string]any{
		"type": "vless", "server": "192.0.2.44", "server_port": float64(443),
		"uuid": "00000000-0000-4000-8000-000000000000",
		"tls":  tlsMetadata, "transport": transportMetadata,
	}

	encoded, err := buildCandidateControlConfig(prepared)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(encoded, &config); err != nil {
		t.Fatal(err)
	}
	inbounds, ok := config["inbounds"].([]any)
	if !ok || len(inbounds) != 1 || inbounds[0].(map[string]any)["type"] != "socks" {
		t.Fatal("control config does not contain exactly one SOCKS inbound")
	}
	outbounds, ok := config["outbounds"].([]any)
	if !ok || len(outbounds) != 1 {
		t.Fatal("control config does not contain exactly one outbound")
	}
	outbound := outbounds[0].(map[string]any)
	if outbound["server"] != "192.0.2.44" || outbound["tag"] != "selected-native-out" {
		t.Fatal("prepared endpoint or selected tag was not preserved")
	}
	if !reflect.DeepEqual(outbound["tls"], tlsMetadata) || !reflect.DeepEqual(outbound["transport"], transportMetadata) {
		t.Fatal("TLS/Reality/WS metadata was changed")
	}
	route, ok := config["route"].(map[string]any)
	if !ok || route["final"] != "selected-native-out" {
		t.Fatal("control route does not select the candidate")
	}
	for _, inbound := range inbounds {
		if inbound.(map[string]any)["type"] == "tun" {
			t.Fatal("control config contains a TUN inbound")
		}
	}
	if prepared["tag"] != nil {
		t.Fatal("builder mutated the prepared candidate")
	}
}

func TestCandidateControlObservationCannotLeakDiagnostics(t *testing.T) {
	private := "private endpoint vless://credential@secret.example"
	got := candidateControlObservation(2, private)
	if got != "lab_observation=real-underlay-control candidate=2 classification=transport" {
		t.Fatalf("unexpected bounded observation %q", got)
	}
	for _, forbidden := range strings.Fields(private) {
		if strings.Contains(got, forbidden) {
			t.Fatal("private diagnostic fragment reached the observation")
		}
	}
	if got := candidateControlObservation(1, "endpoint-resolution"); got != "lab_observation=real-underlay-control candidate=1 classification=endpoint-resolution" {
		t.Fatalf("fixed classification changed: %q", got)
	}
}
