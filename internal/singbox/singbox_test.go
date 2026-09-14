package singbox

import (
	"context"
	"encoding/json"

	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"

	"strings"

	"testing"
	"time"
)

func TestDockerTemplateRoutingInvariants(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "config", "sing-box", "config.tun.json.template"))
	if err != nil {
		t.Fatal(err)
	}
	selectedOutbound := `{"type":"vless","tag":"selected-native-out","server":"203.0.113.10","server_port":443}`
	ruRuleSets := `{"type":"remote","tag":"geoip-ru","format":"binary","url":"https://raw.githubusercontent.com/runetfreedom/russia-v2ray-rules-dat/release/sing-box/rule-set-geoip/geoip-ru.srs","download_detour":"direct-out"},{"type":"remote","tag":"geosite-category-ru","format":"binary","url":"https://raw.githubusercontent.com/runetfreedom/russia-v2ray-rules-dat/release/sing-box/rule-set-geosite/geosite-category-ru.srs","download_detour":"direct-out"}`
	text := strings.ReplaceAll(string(b), "{{SELECTED_NATIVE_OUT_JSON}}", selectedOutbound)
	text = strings.ReplaceAll(text, "{{RU_RULE_SETS_JSON}}", ruRuleSets)
	if strings.Contains(text, "{{SELECTED_NATIVE_OUT_JSON}}") || strings.Contains(text, "{{RU_RULE_SETS_JSON}}") {
		t.Fatal("template placeholder was not replaced")
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(text), &cfg); err != nil {
		t.Fatalf("template is not valid JSON after placeholder substitution: %v", err)
	}

	route := cfg["route"].(map[string]any)
	if route["final"] != "selected-native-out" {
		t.Fatalf("route.final = %#v, want selected-native-out", route["final"])
	}

	rules := route["rules"].([]any)
	if len(rules) < 5 {
		t.Fatalf("route.rules length = %d, want DNS hijack rules, sniff rule, plus RU direct rules", len(rules))
	}
	assertDNSHijackRule(t, rules[0].(map[string]any), "protocol", "dns")
	sniffIdx := assertRouteSniffRule(t, rules, []string{"vpnkit-tun-in", "vpnkit-socks-in"})
	if sniffIdx != 1 {
		t.Fatalf("sniff rule index = %d, want immediately after DNS hijack rules", sniffIdx)
	}

	geoIPIdx := findDirectRuleSetRule(rules, "geoip-ru")
	if geoIPIdx <= sniffIdx {
		t.Fatalf("geoip-ru direct rule index = %d, want after sniff rule index %d", geoIPIdx, sniffIdx)
	}
	geositeIdx := findDirectRuleSetRule(rules, "geosite-category-ru")
	if geositeIdx <= sniffIdx {
		t.Fatalf("geosite-category-ru direct rule index = %d, want after sniff rule index %d", geositeIdx, sniffIdx)
	}

	ruleSets := route["rule_set"].([]any)
	assertRemoteRuleSet(t, ruleSets, "geoip-ru", "rule-set-geoip/geoip-ru.srs")
	assertRemoteRuleSet(t, ruleSets, "geosite-category-ru", "rule-set-geosite/geosite-category-ru.srs")
}

func assertDNSHijackRule(t *testing.T, rule map[string]any, matchKey, matchValue string) {
	t.Helper()
	if rule[matchKey] != matchValue || rule["action"] != "hijack-dns" {
		t.Fatalf("DNS hijack rule = %#v, want %s=%q action=hijack-dns", rule, matchKey, matchValue)
	}
}

func assertRouteSniffRule(t *testing.T, rules []any, wantInbounds []string) int {
	t.Helper()
	for i, raw := range rules {
		rule := raw.(map[string]any)
		if rule["action"] != "sniff" {
			if _, ok := rule["sniff"]; ok {
				t.Fatalf("rule %d uses deprecated sniff field instead of route action syntax: %#v", i, rule)
			}
			continue
		}
		if rule["timeout"] != "1s" {
			t.Fatalf("sniff rule timeout = %#v, want 1s", rule["timeout"])
		}
		gotRaw, ok := rule["inbound"].([]any)
		if !ok {
			t.Fatalf("sniff rule inbound = %#v, want array", rule["inbound"])
		}
		got := make([]string, 0, len(gotRaw))
		for _, v := range gotRaw {
			got = append(got, v.(string))
		}
		if !reflect.DeepEqual(got, wantInbounds) {
			t.Fatalf("sniff rule inbounds = %#v, want %#v", got, wantInbounds)
		}
		return i
	}
	t.Fatal("missing route action sniff rule")
	return -1
}

func findDirectRuleSetRule(rules []any, tag string) int {
	for i, raw := range rules {
		rule := raw.(map[string]any)
		if rule["rule_set"] == tag && rule["outbound"] == "direct-out" {
			return i
		}
	}
	return -1
}

func assertRemoteRuleSet(t *testing.T, ruleSets []any, tag, urlPart string) {
	t.Helper()
	for _, raw := range ruleSets {
		ruleSet := raw.(map[string]any)
		if ruleSet["tag"] != tag {
			continue
		}
		if ruleSet["type"] != "remote" || ruleSet["format"] != "binary" || ruleSet["download_detour"] != "direct-out" {
			t.Fatalf("rule set %s = %#v, want remote binary downloaded via direct-out", tag, ruleSet)
		}
		if !strings.Contains(ruleSet["url"].(string), urlPart) {
			t.Fatalf("rule set %s url = %q, want to contain %q", tag, ruleSet["url"], urlPart)
		}
		return
	}
	t.Fatalf("missing route.rule_set entry for %s", tag)
}

func acknowledgeRequestInTest(req, generation, next string) {
	go func() {
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if b, err := os.ReadFile(req); err == nil && strings.TrimSpace(string(b)) != "" {
				token := strings.TrimSpace(string(b))
				_ = os.WriteFile(generation, []byte(next+"\n"), 0600)
				_ = os.WriteFile(generation+".ack", []byte("token="+token+"\ngeneration="+next+"\nhealth=healthy\n"), 0600)
				_ = os.Remove(req)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
}

func TestSingBoxCheckUsesDirectoryFlag(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "configs")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	old := runCommand
	var got []string
	runCommand = func(_ context.Context, _ string, args ...string) error {
		got = append([]string(nil), args...)
		return nil
	}
	t.Cleanup(func() { runCommand = old })
	if err := CheckContext(context.Background(), "sing-box", dir); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"check", "-C", dir}) {
		t.Fatalf("check args=%v, want check -C %s", got, dir)
	}
}

func TestRunExternalTerminatesBlockingSystemctlDescendant(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "late-restart")
	fake := filepath.Join(dir, "systemctl")
	script := "#!/bin/sh\n( trap '' TERM; sleep 0.2; printf late > \"$1\" ) &\nwhile :; do sleep 1; done\n"
	if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := runExternal(ctx, fake, marker); err == nil {
		t.Fatal("blocking fake systemctl unexpectedly succeeded")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("context cancellation did not terminate systemctl process group: %v", elapsed)
	}
	time.Sleep(350 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("systemctl descendant performed a late restart side effect: %v", err)
	}
}

type singBoxIdentityProbeServer struct {
	address string
	stop    func()
}

func startSingBoxIdentityProbeServer(t *testing.T, mutate func()) singBoxIdentityProbeServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 3)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return
		}
		mutate()
		_, _ = conn.Write([]byte{5, 0})
	}()
	return singBoxIdentityProbeServer{address: listener.Addr().String(), stop: func() {
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("identity probe server did not stop")
		}
	}}
}
