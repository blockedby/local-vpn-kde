//go:build linux

package nativegateway

import (
	"encoding/json"
	"testing"
)

// Captured from `nft -j list table inet vpnkit_native` immediately after the
// real controller created the table in the pinned disposable kernel lab image.
const realNFTListTableFixture = `{
  "nftables": [
    {"metainfo":{"version":"1.0.6","release_name":"Lester Gooch #5","json_schema_version":1}},
    {"table":{"family":"inet","name":"vpnkit_native","handle":1}},
    {"chain":{"family":"inet","table":"vpnkit_native","name":"output_nat","handle":1,"type":"nat","hook":"output","prio":-100,"policy":"accept"}},
    {"chain":{"family":"inet","table":"vpnkit_native","name":"postrouting_nat","handle":2,"type":"nat","hook":"postrouting","prio":100,"policy":"accept"}},
    {"chain":{"family":"inet","table":"vpnkit_native","name":"forward","handle":3,"type":"filter","hook":"forward","prio":0,"policy":"accept"}},
    {"rule":{"family":"inet","table":"vpnkit_native","chain":"output_nat","handle":4,"expr":[
      {"match":{"op":"==","left":{"meta":{"key":"oifname"}},"right":"lo"}},
      {"match":{"op":"==","left":{"payload":{"protocol":"ip","field":"saddr"}},"right":"127.0.0.1"}},
      {"match":{"op":"==","left":{"payload":{"protocol":"ip","field":"daddr"}},"right":"127.0.0.1"}},
      {"match":{"op":"==","left":{"payload":{"protocol":"udp","field":"dport"}},"right":21194}},
      {"dnat":{"family":"ip","addr":"172.30.89.2","port":1194}}
    ]}},
    {"rule":{"family":"inet","table":"vpnkit_native","chain":"postrouting_nat","handle":5,"expr":[
      {"match":{"op":"==","left":{"meta":{"key":"oifname"}},"right":"vpnkit-host"}},
      {"match":{"op":"==","left":{"payload":{"protocol":"ip","field":"saddr"}},"right":"127.0.0.1"}},
      {"match":{"op":"==","left":{"payload":{"protocol":"ip","field":"daddr"}},"right":"172.30.89.2"}},
      {"match":{"op":"==","left":{"payload":{"protocol":"udp","field":"dport"}},"right":1194}},
      {"snat":{"family":"ip","addr":"172.30.89.1"}}
    ]}},
    {"rule":{"family":"inet","table":"vpnkit_native","chain":"postrouting_nat","handle":6,"expr":[
      {"match":{"op":"==","left":{"meta":{"key":"iifname"}},"right":"vpnkit-host"}},
      {"match":{"op":"==","left":{"meta":{"key":"oifname"}},"right":"lab-uplink"}},
      {"match":{"op":"==","left":{"payload":{"protocol":"ip","field":"saddr"}},"right":"172.30.89.2"}},
      {"masquerade":null}
    ]}},
    {"rule":{"family":"inet","table":"vpnkit_native","chain":"forward","handle":7,"expr":[
      {"match":{"op":"==","left":{"meta":{"key":"iifname"}},"right":"vpnkit-host"}},
      {"match":{"op":"==","left":{"meta":{"key":"oifname"}},"right":"lab-uplink"}},
      {"match":{"op":"==","left":{"payload":{"protocol":"ip","field":"saddr"}},"right":"172.30.89.2"}},
      {"match":{"op":"in","left":{"ct":{"key":"state"}},"right":["established","related","new"]}},
      {"accept":null}
    ]}},
    {"rule":{"family":"inet","table":"vpnkit_native","chain":"forward","handle":8,"expr":[
      {"match":{"op":"==","left":{"meta":{"key":"iifname"}},"right":"lab-uplink"}},
      {"match":{"op":"==","left":{"meta":{"key":"oifname"}},"right":"vpnkit-host"}},
      {"match":{"op":"==","left":{"payload":{"protocol":"ip","field":"daddr"}},"right":"172.30.89.2"}},
      {"match":{"op":"in","left":{"ct":{"key":"state"}},"right":["established","related"]}},
      {"accept":null}
    ]}},
    {"rule":{"family":"inet","table":"vpnkit_native","chain":"forward","handle":9,"expr":[
      {"match":{"op":"==","left":{"meta":{"key":"iifname"}},"right":"vpnkit-host"}},
      {"drop":null}
    ]}},
    {"rule":{"family":"inet","table":"vpnkit_native","chain":"forward","handle":10,"expr":[
      {"match":{"op":"==","left":{"meta":{"key":"oifname"}},"right":"vpnkit-host"}},
      {"drop":null}
    ]}}
  ]
}`

func decodeRealNFTFixture(t *testing.T) map[string]any {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal([]byte(realNFTListTableFixture), &document); err != nil {
		t.Fatal(err)
	}
	return document
}

func nftFixtureObjects(t *testing.T, document map[string]any) []any {
	t.Helper()
	objects, ok := document["nftables"].([]any)
	if !ok {
		t.Fatal("fixture nftables is not an array")
	}
	return objects
}

func nftFixtureObservation(document map[string]any) ObservationState {
	return observeEffect(EffectNFTTable, Uplink{Interface: "lab-uplink"}, inspectFacts{nftOK: true, nft: document}).State
}

func TestNFTExactInspectorAcceptsRealListTableSchema(t *testing.T) {
	if state := nftFixtureObservation(decodeRealNFTFixture(t)); state != ObservationExact {
		t.Fatalf("real nft list-table output state = %s, want exact", observationName(state))
	}
}

func TestNFTExactInspectorStillRejectsConflictingRealOutput(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, document map[string]any)
	}{
		{name: "extra rule", mutate: func(t *testing.T, document map[string]any) {
			document["nftables"] = append(nftFixtureObjects(t, document), map[string]any{"rule": map[string]any{
				"family": NFTFamily, "table": NFTTable, "chain": "forward", "expr": []any{map[string]any{"accept": nil}},
			}})
		}},
		{name: "changed selector", mutate: func(t *testing.T, document map[string]any) {
			for _, object := range nftFixtureObjects(t, document) {
				rule, ok := object.(map[string]any)["rule"].(map[string]any)
				if !ok || rule["handle"] != float64(6) {
					continue
				}
				expressions := rule["expr"].([]any)
				expressions[1].(map[string]any)["match"].(map[string]any)["right"] = "other-uplink"
				return
			}
			t.Fatal("selector fixture rule not found")
		}},
		{name: "changed policy", mutate: func(t *testing.T, document map[string]any) {
			for _, object := range nftFixtureObjects(t, document) {
				chain, ok := object.(map[string]any)["chain"].(map[string]any)
				if ok && chain["name"] == "forward" {
					chain["policy"] = "drop"
					return
				}
			}
			t.Fatal("forward chain not found")
		}},
		{name: "changed dnat address", mutate: func(t *testing.T, document map[string]any) {
			changeNFTAddress(t, document, "dnat", "172.30.89.3")
		}},
		{name: "changed snat address", mutate: func(t *testing.T, document map[string]any) {
			changeNFTAddress(t, document, "snat", "172.30.89.3")
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			document := decodeRealNFTFixture(t)
			test.mutate(t, document)
			if state := nftFixtureObservation(document); state != ObservationConflict {
				t.Fatalf("conflicting nft output state = %s, want conflicting", observationName(state))
			}
		})
	}
}

func changeNFTAddress(t *testing.T, document map[string]any, kind, address string) {
	t.Helper()
	for _, object := range nftFixtureObjects(t, document) {
		rule, ok := object.(map[string]any)["rule"].(map[string]any)
		if !ok {
			continue
		}
		for _, expression := range rule["expr"].([]any) {
			nat, ok := expression.(map[string]any)[kind].(map[string]any)
			if ok {
				nat["addr"] = address
				return
			}
		}
	}
	t.Fatalf("%s expression not found", kind)
}
