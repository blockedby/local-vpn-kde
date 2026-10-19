//go:build linux

package localvpn

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	nativeContainerRuleSetDir = "/etc/sing-box/rule-sets/"
	nativeRuleSetDir          = "/etc/local-vpn-kde/sing-box/rule-sets/"
)

// nativeSingBoxConfig adapts only the rule-set paths emitted by buildConfig.
// Other strings are deliberately not searched or replaced: a generated config
// may contain credentials or policy data that must remain byte-for-byte
// unchanged as values.
func nativeSingBoxConfig(data []byte) ([]byte, error) {
	var config map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("parse sing-box config: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("parse sing-box config: multiple JSON values")
		}
		return nil, fmt.Errorf("parse sing-box config: %w", err)
	}
	if config == nil {
		return nil, errors.New("sing-box config must be a JSON object")
	}

	rawRoute, ok := config["route"]
	if !ok {
		return marshalNativeSingBoxConfig(config)
	}
	route, ok := rawRoute.(map[string]any)
	if !ok {
		return nil, errors.New("sing-box route must be a JSON object")
	}
	rawRuleSets, ok := route["rule_set"]
	if !ok {
		return marshalNativeSingBoxConfig(config)
	}
	ruleSets, ok := rawRuleSets.([]any)
	if !ok {
		return nil, errors.New("sing-box route.rule_set must be a JSON array")
	}
	for index, rawRuleSet := range ruleSets {
		ruleSet, ok := rawRuleSet.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("sing-box route.rule_set[%d] must be a JSON object", index)
		}
		rawPath, hasPath := ruleSet["path"]
		if !hasPath {
			if ruleSet["type"] == "local" {
				return nil, fmt.Errorf("sing-box route.rule_set[%d] local rule set has no path", index)
			}
			continue
		}
		if ruleSet["type"] != "local" {
			return nil, fmt.Errorf("unexpected path in sing-box route.rule_set[%d]", index)
		}
		path, ok := rawPath.(string)
		if !ok {
			return nil, fmt.Errorf("sing-box route.rule_set[%d] path must be a string", index)
		}
		adapted, err := nativeRuleSetPath(path)
		if err != nil {
			return nil, fmt.Errorf("sing-box route.rule_set[%d]: %w", index, err)
		}
		ruleSet["path"] = adapted
	}
	return marshalNativeSingBoxConfig(config)
}

func nativeRuleSetPath(path string) (string, error) {
	if !strings.HasPrefix(path, nativeContainerRuleSetDir) {
		return "", fmt.Errorf("unexpected local rule-set path %q", path)
	}
	name := strings.TrimPrefix(path, nativeContainerRuleSetDir)
	if len(name) <= len(".json") || !strings.HasSuffix(name, ".json") {
		return "", fmt.Errorf("unexpected local rule-set path %q", path)
	}
	for index, char := range name[:len(name)-len(".json")] {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '_' && char != '-' && char != '.' {
			return "", fmt.Errorf("unexpected local rule-set path %q", path)
		}
		if index == 0 && (char == '.' || char == '_' || char == '-') {
			return "", fmt.Errorf("unexpected local rule-set path %q", path)
		}
	}
	return nativeRuleSetDir + name, nil
}

func marshalNativeSingBoxConfig(config map[string]any) ([]byte, error) {
	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode native sing-box config: %w", err)
	}
	return append(encoded, '\n'), nil
}

const nativeVibeConfigValue = `subscription_file: /etc/local-vpn-kde/vibe-vpn/sub_url
runtime: singbox
sing_box_bin: /usr/libexec/local-vpn-kde/sing-box
sing_box_config: /var/lib/local-vpn-kde/sing-box/config.json
sing_box_service: vpnkit-supervised-sing-box
sing_box_restart_mode: request-file
sing_box_restart_file: /run/local-vpn-kde/restart-sing-box
# Daemon/manual apply must wait until the entrypoint consumes this exact
# request token, completes the full runtime health predicate, advances this
# generation marker, and publishes the matching health acknowledgement.
sing_box_restart_ack_generation_file: /run/local-vpn-kde/sing-box-generation
sing_box_restart_ack_file: /run/local-vpn-kde/sing-box-generation.ack
sing_box_restart_ack_timeout: 60s
state_dir: /var/lib/local-vpn-kde/vibe-vpn
production_socks: 127.0.0.1:2080
test_socks: 127.0.0.1:18080
test_url: https://proof.ovh.net/files/10Mb.dat
test_limit_kib: 64
timeout_seconds: 12
`

func nativeVibeConfig() string { return nativeVibeConfigValue }
