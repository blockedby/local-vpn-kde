//go:build linux

package localvpn

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestNativeSingBoxConfigAdaptsBuildConfig(t *testing.T) {
	templatePath := renderFixture(t).Template
	template, err := os.ReadFile(templatePath)
	if err != nil {
		t.Fatal(err)
	}

	for _, policy := range []string{"smart", "strict"} {
		for _, ruleSets := range []string{"remote", "local-fixture"} {
			t.Run(policy+"/"+ruleSets, func(t *testing.T) {
				o := RenderOptions{Policy: policy, RuleSets: ruleSets, Outbound: "subscription"}
				generated, err := buildConfig(template, o)
				if err != nil {
					t.Fatal(err)
				}
				adapted, err := nativeSingBoxConfig(generated)
				if err != nil {
					t.Fatal(err)
				}

				var before, after map[string]any
				if err := json.Unmarshal(generated, &before); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(adapted, &after); err != nil {
					t.Fatal(err)
				}
				assertNativeRuleSetPaths(t, before, after)
				dns := after["dns"].(map[string]any)["servers"].([]any)
				var found bool
				for _, raw := range dns {
					server := raw.(map[string]any)
					if server["tag"] == "direct-dns" {
						found = true
						if server["type"] != "udp" || server["server"] != "8.8.8.8" {
							t.Fatal("namespace bootstrap DNS still depends on host resolver")
						}
					}
				}
				if !found {
					t.Fatal("bootstrap resolver missing")
				}
				before["dns"] = after["dns"]
				if !reflect.DeepEqual(before, after) {
					t.Fatalf("native adaptation changed config semantics:\nbefore=%v\nafter=%v", before, after)
				}
			})
		}
	}
}

func assertNativeRuleSetPaths(t *testing.T, before, after map[string]any) {
	t.Helper()
	routeBefore := before["route"].(map[string]any)
	routeAfter := after["route"].(map[string]any)
	setsBefore := routeBefore["rule_set"].([]any)
	setsAfter := routeAfter["rule_set"].([]any)
	if len(setsBefore) != len(setsAfter) {
		t.Fatalf("rule_set length changed: before=%d after=%d", len(setsBefore), len(setsAfter))
	}
	for index := range setsBefore {
		beforeSet := setsBefore[index].(map[string]any)
		afterSet := setsAfter[index].(map[string]any)
		beforePath, hasPath := beforeSet["path"]
		afterPath, adapted := afterSet["path"]
		if !hasPath {
			if adapted {
				t.Errorf("rule_set[%d] gained a path", index)
			}
			continue
		}
		path, ok := beforePath.(string)
		if !ok {
			t.Fatalf("rule_set[%d] generated non-string path", index)
		}
		expected, err := nativeRuleSetPath(path)
		if err != nil {
			t.Fatalf("generated path %q was not adaptable: %v", path, err)
		}
		if afterPath != expected {
			t.Errorf("rule_set[%d] path=%v, want %q", index, afterPath, expected)
		}
		delete(beforeSet, "path")
		delete(afterSet, "path")
	}
}

func TestNativeSingBoxConfigDoesNotRewritePolicyOrCredentials(t *testing.T) {
	input := []byte(`{
  "policy": {"mode": "strict", "credential": "/etc/sing-box/rule-sets/not-a-rule.json"},
  "dns": {"servers": [{"tag": "remote", "server": "1.1.1.1"}]},
  "inbounds": [{"tag": "in", "password": "credential-/etc/sing-box/rule-sets/not-a-rule.json"}],
  "outbounds": [{"tag": "out", "server": "198.51.100.10"}],
  "route": {
    "rules": [{"rule_set": "known", "outbound": "out"}],
    "rule_set": [{"type": "local", "tag": "known", "path": "/etc/sing-box/rule-sets/known.json"}]
  }
}`)
	got, err := nativeSingBoxConfig(input)
	if err != nil {
		t.Fatal(err)
	}
	var want, actual map[string]any
	if err := json.Unmarshal(input, &want); err != nil {
		t.Fatal(err)
	}
	want["route"].(map[string]any)["rule_set"].([]any)[0].(map[string]any)["path"] = "/etc/local-vpn-kde/sing-box/rule-sets/known.json"
	if err := json.Unmarshal(got, &actual); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, actual) {
		t.Fatalf("unexpected non-path mutation:\nwant=%v\n got=%v", want, actual)
	}
}

func TestNativeSingBoxConfigRejectsMalformedAndUnexpectedPaths(t *testing.T) {
	cases := []string{
		`{"route":{"rule_set":[`,
		`{"route":{"rule_set":[{"type":"local","path":"/tmp/rules.json"}]}}`,
		`{"route":{"rule_set":[{"type":"local","path":"/etc/sing-box/rule-sets/../secret.json"}]}}`,
		`{"route":{"rule_set":[{"type":"local","path":"/etc/sing-box/rule-sets/rules.yaml"}]}}`,
		`{"route":{"rule_set":[{"type":"remote","path":"/etc/sing-box/rule-sets/rules.json"}]}}`,
		`{"route":{"rule_set":[{"type":"local"}]}}`,
		`{"route":{"rule_set":"not-an-array"}}`,
	}
	for index, input := range cases {
		t.Run(string(rune('a'+index)), func(t *testing.T) {
			if _, err := nativeSingBoxConfig([]byte(input)); err == nil {
				t.Fatalf("accepted unexpected config %s", input)
			}
		})
	}
}

func TestNativeVibeConfigUsesFixedInstalledPaths(t *testing.T) {
	config := nativeVibeConfig()
	for _, line := range []string{
		"subscription_file: /etc/local-vpn-kde/vibe-vpn/sub_url",
		"sing_box_bin: /usr/libexec/local-vpn-kde/sing-box",
		"sing_box_config: /var/lib/local-vpn-kde/sing-box/config.json",
		"sing_box_restart_file: /run/local-vpn-kde/restart-sing-box",
		"sing_box_restart_ack_generation_file: /run/local-vpn-kde/sing-box-generation",
		"sing_box_restart_ack_file: /run/local-vpn-kde/sing-box-generation.ack",
		"state_dir: /var/lib/local-vpn-kde/vibe-vpn",
	} {
		if !strings.Contains(config, line) {
			t.Errorf("native config missing %q", line)
		}
	}
	for _, oldPath := range []string{
		"/etc/vibe-vpn-input/sub_url",
		"/usr/local/bin/sing-box",
		"/var/lib/vpnkit/sing-box/config.json",
		"/run/vpnkit/restart-sing-box",
		"/run/vpnkit/sing-box-generation",
		"/var/lib/vibe-vpn",
	} {
		if strings.Contains(config, oldPath) {
			t.Errorf("native config retains old path %q", oldPath)
		}
	}
}
