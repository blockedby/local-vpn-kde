package localvpn

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

type RenderOptions struct {
	Template, Base, Policy, RuleSets, Outbound string
	AllowMissingSubscription, Fixture          bool
	// afterRead is an internal deterministic race seam, never an external command.
	afterRead func() error
}

// Render holds each destination directory open until all outputs are installed.
// Renaming or replacing a path during rendering cannot redirect writes or chmod.
func Render(o RenderOptions) error {
	if o.Policy != "strict" && o.Policy != "smart" {
		return errors.New("invalid policy")
	}
	if o.RuleSets != "remote" && o.RuleSets != "local-fixture" {
		return errors.New("invalid rule sets")
	}
	if o.Outbound != "subscription" && o.Outbound != "direct-fixture" {
		return errors.New("invalid outbound")
	}
	if o.Outbound == "direct-fixture" && (!o.Fixture || o.RuleSets != "local-fixture") {
		return errors.New("direct outbound requires a local fixture")
	}
	if err := validateSecretTree(o.Base, func(path string) bool {
		switch path {
		case "rendered/sing-box/config.json", "rendered/sing-box/rule-sets/vpnkit-adblock.json",
			"rendered/sing-box/rule-sets/vpnkit-dev-direct.json", "rendered/sing-box/rule-sets/geoip-ru.json",
			"rendered/sing-box/rule-sets/geosite-category-ru.json", "rendered/vibe-vpn/config.yaml", "rendered/vibe-vpn/sub_url":
			return true
		}
		return false
	}); err != nil {
		return err
	}
	base, err := directory(o.Base, true, true)
	if err != nil {
		return err
	}
	fds := []int{base}
	defer func() {
		for i := len(fds) - 1; i >= 0; i-- {
			unix.Close(fds[i])
		}
	}()
	open := func(parent int, name string) (int, error) {
		fd, e := childDirectory(parent, name, true, true)
		if e == nil {
			fds = append(fds, fd)
		}
		return fd, e
	}
	source, err := open(base, "vibe-vpn")
	if err != nil {
		return err
	}
	rendered, err := open(base, "rendered")
	if err != nil {
		return err
	}
	singbox, err := open(rendered, "sing-box")
	if err != nil {
		return err
	}
	rules, err := open(singbox, "rule-sets")
	if err != nil {
		return err
	}
	vibe, err := open(rendered, "vibe-vpn")
	if err != nil {
		return err
	}
	// Validate the raw template path before filepath.Dir can clean traversal.
	for _, part := range strings.Split(o.Template, "/") {
		if part == "." || part == ".." {
			return errors.New("canonical template path required")
		}
	}
	templateDir, err := directory(filepath.Dir(o.Template), false, false)
	if err != nil {
		return err
	}
	defer unix.Close(templateDir)
	template, err := readAt(templateDir, filepath.Base(o.Template))
	if err != nil {
		return err
	}
	config, err := buildConfig(template, o)
	if err != nil {
		return err
	}
	sourceRules, err := childDirectory(templateDir, "rule-sets", false, false)
	if err != nil {
		return err
	}
	ruleFile := os.NewFile(uintptr(sourceRules), "rule-sets")
	defer ruleFile.Close()
	names, err := ruleFile.Readdirnames(-1)
	if err != nil {
		return err
	}
	sort.Strings(names)
	type output struct {
		name string
		data []byte
	}
	outputs := []output{}
	for _, name := range names {
		if strings.HasSuffix(name, ".json") {
			data, e := readAt(sourceRules, name)
			if e != nil {
				return e
			}
			outputs = append(outputs, output{name, data})
		}
	}
	subscription, err := readAt(source, "sub_url")
	if err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	if len(subscription) == 0 && !o.AllowMissingSubscription {
		return errors.New("subscription missing")
	}
	if o.afterRead != nil {
		if err = o.afterRead(); err != nil {
			return err
		}
	}
	for _, fd := range fds {
		if err = checkDirectory(fd, true); err != nil {
			return err
		}
		if err = unix.Fchmod(fd, 0700); err != nil {
			return err
		}
		if err = unix.Fsync(fd); err != nil {
			return err
		}
	}
	if err = atomicWrite(singbox, "config.json", config); err != nil {
		return err
	}
	for _, out := range outputs {
		if err = atomicWrite(rules, out.name, out.data); err != nil {
			return err
		}
	}
	if o.RuleSets == "local-fixture" {
		if err = atomicWrite(rules, "geoip-ru.json", []byte("{\"version\":1,\"rules\":[{\"ip_cidr\":[\"5.0.0.0/8\"]}]}\n")); err != nil {
			return err
		}
		if err = atomicWrite(rules, "geosite-category-ru.json", []byte("{\"version\":1,\"rules\":[{\"domain_suffix\":[\"ru\"]}]}\n")); err != nil {
			return err
		}
	}
	if err = atomicWrite(vibe, "config.yaml", []byte(vibeConfig)); err != nil {
		return err
	}
	if len(subscription) > 0 {
		return atomicWrite(vibe, "sub_url", subscription)
	}
	return nil
}

func buildConfig(template []byte, o RenderOptions) ([]byte, error) {
	type object = map[string]any
	bootstrap := object{"type": "vless", "tag": "selected-native-out", "server": "192.0.2.1", "server_port": 443, "uuid": "00000000-0000-4000-8000-000000000000", "tls": object{"enabled": true, "server_name": "bootstrap.example.invalid"}}
	if o.Outbound == "direct-fixture" {
		bootstrap = object{"type": "direct", "tag": "selected-native-out"}
	}
	sets := []string{}
	for _, tag := range []string{"geoip-ru", "geosite-category-ru"} {
		category := "geoip"
		if strings.HasPrefix(tag, "geosite") {
			category = "geosite"
		}
		set := object{"type": "remote", "tag": tag, "format": "binary", "url": "https://raw.githubusercontent.com/runetfreedom/russia-v2ray-rules-dat/release/sing-box/rule-set-" + category + "/" + tag + ".srs", "download_detour": "direct-out"}
		if o.RuleSets == "local-fixture" {
			set = object{"type": "local", "tag": tag, "format": "source", "path": "/etc/sing-box/rule-sets/" + tag + ".json"}
		}
		data, _ := json.Marshal(set)
		sets = append(sets, string(data))
	}
	outbound, _ := json.Marshal(bootstrap)
	text := strings.ReplaceAll(string(template), "{{SELECTED_NATIVE_OUT_JSON}}", string(outbound))
	text = strings.ReplaceAll(text, "{{RU_RULE_SETS_JSON}}", strings.Join(sets, ","))
	var config object
	if err := json.Unmarshal([]byte(text), &config); err != nil {
		return nil, err
	}
	dns, ok := config["dns"].(map[string]any)
	if !ok {
		return nil, errors.New("missing DNS configuration")
	}
	servers, ok := dns["servers"].([]any)
	if !ok {
		return nil, errors.New("missing DNS servers")
	}
	for _, spec := range []struct{ tag, ip, host string }{{"remote-dns", "1.1.1.1", "cloudflare-dns.com"}, {"remote-dns-fallback", "8.8.8.8", "dns.google"}} {
		found := false
		for _, value := range servers {
			server, ok := value.(map[string]any)
			if !ok || server["tag"] != spec.tag {
				continue
			}
			found = true
			for k, v := range (object{"type": "https", "server": spec.ip, "server_port": 443, "path": "/dns-query", "tls": object{"enabled": true, "server_name": spec.host}}) {
				server[k] = v
			}
			if o.Outbound == "direct-fixture" {
				delete(server, "detour")
			}
		}
		if !found {
			return nil, errors.New("missing required DNS tag")
		}
	}
	dns["final"] = "remote-dns"
	if o.Policy == "strict" {
		route, ok := config["route"].(map[string]any)
		if !ok {
			return nil, errors.New("missing route configuration")
		}
		filtered := []any{}
		rules, _ := route["rules"].([]any)
		for _, value := range rules {
			rule, ok := value.(map[string]any)
			if !ok {
				return nil, errors.New("invalid route rule")
			}
			if rule["protocol"] == "dns" || rule["action"] == "sniff" {
				filtered = append(filtered, rule)
			}
		}
		route["rules"] = filtered
		route["rule_set"] = []any{}
	}
	data, err := json.MarshalIndent(config, "", "  ")
	return append(data, '\n'), err
}

const vibeConfig = `subscription_file: /etc/vibe-vpn-input/sub_url
runtime: singbox
sing_box_bin: /usr/local/bin/sing-box
sing_box_config: /var/lib/vpnkit/sing-box/config.json
sing_box_service: vpnkit-supervised-sing-box
sing_box_restart_mode: request-file
sing_box_restart_file: /run/vpnkit/restart-sing-box
# Daemon/manual apply must wait until the entrypoint consumes this exact
# request token, completes the full runtime health predicate, advances this
# generation marker, and publishes the matching health acknowledgement.
sing_box_restart_ack_generation_file: /run/vpnkit/sing-box-generation
sing_box_restart_ack_file: /run/vpnkit/sing-box-generation.ack
sing_box_restart_ack_timeout: 60s
state_dir: /var/lib/vibe-vpn
production_socks: 127.0.0.1:2080
test_socks: 127.0.0.1:18080
test_url: https://proof.ovh.net/files/10Mb.dat
test_limit_kib: 64
timeout_seconds: 12
`
