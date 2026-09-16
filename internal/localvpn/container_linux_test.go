package localvpn

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixtureDNSReply(query []byte) []byte {
	// Fixed A-record fixture. Preserve the question and answer it with
	// a compressed owner name pointing at its offset in the DNS header.
	if len(query) < 17 || query[4] != 0 || query[5] != 1 {
		return nil
	}
	end := 12
	for end < len(query) && query[end] != 0 {
		if query[end] > 63 {
			end = len(query)
			break
		}
		end += int(query[end]) + 1
	}
	end += 5
	if end > len(query) {
		return nil
	}
	data := append([]byte(nil), query[:end]...)
	data[2], data[3] = 0x81, 0x80
	data[6], data[7] = 0, 1
	data[8], data[9], data[10], data[11] = 0, 0, 0, 0
	data = append(data, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4, 192, 0, 2, 42)
	return data
}

// This helper is executed only inside the disposable target container. Its
// network has no external gateway, so a host VPN cannot carry test traffic.
func TestContainerTarget(t *testing.T) {
	if os.Getenv("LOCAL_VPN_CONTAINER_TARGET") != "1" {
		return
	}
	// The DNS backend exists only on the isolated test network. Client requests
	// to a public DNS address must be intercepted to reach this fixture.
	udp, err := net.ListenPacket("udp", ":5353")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, peer, err := udp.ReadFrom(buf)
			if err != nil {
				return
			}
			data := fixtureDNSReply(buf[:n])
			if data == nil {
				continue
			}
			udp.WriteTo(data, peer)
		}
	}()
	if os.Getenv("LOCAL_VPN_CONTAINER_GRPC_TARGET") == "1" {
		cfg := `{"inbounds":[{"type":"vless","listen":"0.0.0.0","listen_port":8443,"users":[{"uuid":"11111111-1111-4111-8111-111111111111"}],"transport":{"type":"grpc","service_name":"dns-lab"}}],"outbounds":[{"type":"direct","tag":"direct"}],"route":{"final":"direct"}}`
		path := filepath.Join(t.TempDir(), "grpc.json")
		if err := os.WriteFile(path, []byte(cfg), 0600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("/usr/local/bin/sing-box", "run", "-c", path)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { cmd.Process.Kill(); cmd.Wait() }()
		tlsServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			query, err := io.ReadAll(io.LimitReader(r.Body, 4096))
			if err != nil {
				http.Error(w, "bad query", 400)
				return
			}
			answer := fixtureDNSReply(query)
			if answer == nil {
				http.Error(w, "bad DNS", 400)
				return
			}
			w.Header().Set("Content-Type", "application/dns-message")
			w.Write(answer)
		}))
		tlsServer.Listener.Close()
		tlsServer.Listener, err = net.Listen("tcp", ":8444")
		if err != nil {
			t.Fatal(err)
		}
		tlsServer.EnableHTTP2 = true
		tlsServer.StartTLS()
		defer tlsServer.Close()
	}
	server := &http.Server{Addr: ":8080", ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("local-vpn-isolated-target\n")) })}
	if err := server.ListenAndServe(); err != nil {
		t.Fatal(err)
	}
}

func TestContainerDNSProbe(t *testing.T) {
	if os.Getenv("LOCAL_VPN_CONTAINER_DNS_PROBE") != "1" {
		return
	}
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, "8.8.8.8:53")
	}}
	for i := 0; i < 12; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		addresses, err := resolver.LookupIP(ctx, "ip4", fmt.Sprintf("dns-probe-%d.example", i))
		cancel()
		if err != nil || len(addresses) != 1 || addresses[0].String() != "192.0.2.42" {
			t.Fatalf("DNS query %d through OpenVPN failed", i)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestContainerDataPath(t *testing.T) {
	if os.Getenv("LOCAL_VPN_CONTAINER_TEST") != "1" {
		t.Skip("set LOCAL_VPN_CONTAINER_TEST=1 to run disposable Docker acceptance")
	}
	for _, grpc := range []bool{false, true} {
		t.Run(fmt.Sprintf("grpc=%v", grpc), func(t *testing.T) { testContainerDataPath(t, grpc) })
	}
}
func testContainerDataPath(t *testing.T, grpc bool) {
	if os.Getenv("LOCAL_VPN_CONTAINER_TEST") != "1" {
		t.Skip("set LOCAL_VPN_CONTAINER_TEST=1 to run disposable Docker acceptance")
	}
	image := os.Getenv("LOCAL_VPN_CONTAINER_IMAGE")
	if image == "" {
		image = "local-vpn-kde:latest"
	}
	var random [6]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	prefix := "local-vpn-go-" + hex.EncodeToString(random[:])
	network := prefix + "-net"
	run := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		// Docker errors may contain runtime details; expose only the fixed operation.
		if err != nil {
			return "", fmt.Errorf("docker %s failed: %w", args[0], err)
		}
		return strings.TrimSpace(string(output)), nil
	}
	must := func(args ...string) string {
		t.Helper()
		value, err := run(args...)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	must("image", "inspect", image)
	netID := must("network", "create", "--internal", "--label", "com.vpnkit.test=go-migration", network)
	t.Cleanup(func() {
		if _, err := run("network", "rm", netID); err != nil {
			t.Error(err)
		}
	})
	isolated := must("network", "inspect", "--format", "{{.Internal}}", netID)
	if isolated != "true" {
		t.Fatal("fixture network is not isolated")
	}
	base := t.TempDir()
	templates, _ := filepath.Abs("../../config/openvpn")
	template, _ := filepath.Abs("../../config/sing-box/config.tun.json.template")
	if err := Assets(AssetOptions{Base: base, TemplateDir: templates, Endpoint: "127.0.0.1", Port: 1194, DNS: "8.8.8.8", CertificateDays: 1}); err != nil {
		t.Fatal(err)
	}
	if err := Render(RenderOptions{Template: template, Base: base, Policy: "strict", RuleSets: "local-fixture", Outbound: "direct-fixture", Fixture: true, AllowMissingSubscription: true}); err != nil {
		t.Fatal(err)
	}
	// The test binary also provides the network target, avoiding Python, an
	// external web server, and downloads during acceptance.
	binary := filepath.Join(base, "acceptance")
	build := exec.Command("go", "test", "-c", "-o", binary, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("acceptance build: %v %s", err, output)
	}
	launch := func(name string, args ...string) string {
		t.Helper()
		common := []string{"run", "-d", "--name", prefix + "-" + name, "--network", network, "--label", "com.vpnkit.test=go-migration"}
		id := must(append(common, args...)...)
		t.Cleanup(func() {
			if _, err := run("rm", "-f", id); err != nil {
				t.Error(err)
			}
		})
		return id
	}
	gatewayArgs := []string{}
	useImageBinary := os.Getenv("LOCAL_VPN_CONTAINER_USE_IMAGE_BINARY") == "1"
	// Artifact acceptance uses the image's binary and ENTRYPOINT unchanged.
	// The default development loop can still mount a fresh binary into an
	// existing dependencies image without rebuilding that image each time.
	if !useImageBinary {
		runtimeBinary := filepath.Join(base, "local-vpn-kde.bin")
		runtimeBuild := exec.Command("go", "build", "-o", runtimeBinary, "../../cmd/local-vpn-kde")
		runtimeBuild.Env = append(os.Environ(), "CGO_ENABLED=0")
		if output, err := runtimeBuild.CombinedOutput(); err != nil {
			t.Fatalf("runtime build: %v %s", err, output)
		}
		gatewayArgs = append(gatewayArgs, "--entrypoint", "/usr/local/bin/local-vpn-kde", "-v", runtimeBinary+":/usr/local/bin/local-vpn-kde:ro")
	}
	target := launch("target", "-e", fmt.Sprintf("LOCAL_VPN_CONTAINER_GRPC_TARGET=%d", map[bool]int{true: 1, false: 0}[grpc]), "-e", "LOCAL_VPN_CONTAINER_TARGET=1", "-v", binary+":/acceptance:ro", "--entrypoint", "/acceptance", image, "-test.run=^TestContainerTarget$")
	address := func(id string) string {
		t.Helper()
		raw := must("inspect", "--format", "{{json .NetworkSettings.Networks}}", id)
		var networks map[string]struct{ IPAddress string }
		if err := json.Unmarshal([]byte(raw), &networks); err != nil {
			t.Fatal(err)
		}
		ip := networks[network].IPAddress
		if net.ParseIP(ip) == nil {
			t.Fatal("invalid fixture address")
		}
		return ip
	}
	targetIP := address(target)
	configPath := filepath.Join(base, "rendered/sing-box/config.json")
	configBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var rendered map[string]any
	if err = json.Unmarshal(configBytes, &rendered); err != nil {
		t.Fatal(err)
	}
	dns := rendered["dns"].(map[string]any)
	dnsALPN := dns["servers"].([]any)[0].(map[string]any)["tls"].(map[string]any)["alpn"]
	dns["servers"] = []any{map[string]any{"type": "udp", "tag": "remote-dns", "server": targetIP, "server_port": 5353}, map[string]any{"type": "udp", "tag": "remote-dns-fallback", "server": targetIP, "server_port": 5353}, map[string]any{"type": "local", "tag": "direct-dns"}}
	if grpc {
		for _, value := range rendered["outbounds"].([]any) {
			o := value.(map[string]any)
			if o["tag"] == "selected-native-out" {
				o["type"] = "vless"
				o["server"] = targetIP
				o["server_port"] = 8443
				o["uuid"] = "11111111-1111-4111-8111-111111111111"
				o["transport"] = map[string]any{"type": "grpc", "service_name": "dns-lab"}
			}
		}
		for _, value := range dns["servers"].([]any) {
			d := value.(map[string]any)
			if d["tag"] == "direct-dns" {
				continue
			}
			d["type"] = "https"
			d["server_port"] = 8444
			d["path"] = "/dns-query"
			d["tls"] = map[string]any{"enabled": true, "insecure": true, "alpn": dnsALPN}
			d["detour"] = "selected-native-out"
		}
	}
	configBytes, err = json.Marshal(rendered)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(configPath, configBytes, 0600); err != nil {
		t.Fatal(err)
	}

	gatewayArgs = append(gatewayArgs, "--cap-add", "NET_ADMIN", "--device", "/dev/net/tun", "--sysctl", "net.ipv4.ip_forward=1", "-e", "VPNKIT_ROUTING_MODE=tun", "-e", "VPNKIT_IPV6_POLICY=block", "-e", "OVPN_CIDR=10.89.0.0/24", "-v", filepath.Join(base, "rendered/openvpn")+":/etc/openvpn:ro", "-v", filepath.Join(base, "rendered/sing-box")+":/etc/sing-box:ro", image)
	if !useImageBinary {
		gatewayArgs = append(gatewayArgs, "container")
	}
	gateway := launch("gateway", gatewayArgs...)
	gatewayIP := address(gateway)
	deadline := time.Now().Add(35 * time.Second)
	for {
		if _, err := run("exec", gateway, "/usr/local/bin/local-vpn-kde", "health"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture gateway did not become healthy")
		}
		time.Sleep(200 * time.Millisecond)
	}
	profilePath := filepath.Join(base, "openvpn/client/vpnkit-local.ovpn")
	profile, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	// This substitution is confined to a generated fixture profile. The actual
	// application continues to generate loopback-only KDE profiles.
	profile = []byte(strings.Replace(string(profile), "remote 127.0.0.1 1194", "remote "+gatewayIP+" 1194", 1))
	if err = os.WriteFile(profilePath, profile, 0600); err != nil {
		t.Fatal(err)
	}
	client := launch("client", "-v", binary+":/acceptance:ro", "--cap-add", "NET_ADMIN", "--device", "/dev/net/tun", "-v", profilePath+":/client.ovpn:ro", "--entrypoint", "openvpn", image, "--config", "/client.ovpn", "--dev", "tun0")
	deadline = time.Now().Add(25 * time.Second)
	for {
		if _, err = run("exec", client, "ip", "link", "show", "tun0"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("OpenVPN handshake failed")
		}
		time.Sleep(200 * time.Millisecond)
	}
	DNSRoute := must("exec", client, "ip", "-4", "route", "get", "8.8.8.8")
	if !strings.Contains(DNSRoute, "dev tun0") {
		t.Fatal("DNS probe bypasses OpenVPN")
	}
	must("exec", "-e", "LOCAL_VPN_CONTAINER_DNS_PROBE=1", client, "/acceptance", "-test.run=^TestContainerDNSProbe$")
	// A /32 overrides the client's directly connected fixture subnet. Assert
	// the selected device before fetching; otherwise this could pass without VPN.
	must("exec", client, "ip", "route", "replace", targetIP+"/32", "via", "10.89.0.1", "dev", "tun0")
	route := must("exec", client, "ip", "-j", "route", "get", targetIP)
	var routes []struct{ Dev string }
	if err = json.Unmarshal([]byte(route), &routes); err != nil || len(routes) != 1 || routes[0].Dev != "tun0" {
		t.Fatal("target bypasses the OpenVPN tunnel")
	}
	payload := must("exec", client, "curl", "--noproxy", "*", "--fail", "--silent", "--max-time", "10", "http://"+targetIP+":8080/")
	if payload != "local-vpn-isolated-target" {
		t.Fatal("incorrect target response")
	}
	// A DNS change exercises the same token/generation/health handshake used
	// for server selection while the client's OpenVPN connection stays up.
	before := must("exec", gateway, "cat", "/run/vpnkit/sing-box-generation")
	ipv6Before := must("exec", gateway, "ip6tables", "-S", "OVPN_IPV6_BLOCK")
	must("exec", gateway, "/usr/local/bin/local-vpn-kde", "dns-switch", "remote-dns-fallback")
	after := must("exec", gateway, "cat", "/run/vpnkit/sing-box-generation")
	if before == after {
		t.Fatal("native restart did not advance generation")
	}
	must("exec", gateway, "/usr/local/bin/local-vpn-kde", "health")
	if _, err := run("exec", gateway, "test", "-e", "/run/vpnkit/restart-sing-box"); err == nil {
		t.Fatal("restart request not consumed")
	}
	if response := must("exec", client, "curl", "--noproxy", "*", "--fail", "--silent", "--max-time", "10", "http://"+targetIP+":8080/"); response != payload {
		t.Fatal("client traffic did not recover after restart")
	}
	// Losing the TUN table must not fall through to a direct default route.
	must("exec", gateway, "ip", "route", "flush", "table", "101")
	if _, err := run("exec", gateway, "ip", "route", "get", targetIP, "from", "10.89.0.2", "iif", "tun0"); err == nil {
		t.Fatal("missing TUN route fell through to the physical route")
	}
	must("exec", gateway, "/usr/local/bin/local-vpn-kde", "routing")
	// The barrier must fail health and block the client rather than allowing a
	// direct forwarding fallback. Reapplying native routing restores service.
	must("exec", gateway, "/usr/local/bin/local-vpn-kde", "routing", "--install-fail-closed-barrier")
	if _, err := run("exec", gateway, "/usr/local/bin/local-vpn-kde", "health"); err == nil {
		t.Fatal("health accepted blocked routing")
	}
	if _, err := run("exec", client, "curl", "--noproxy", "*", "--fail", "--silent", "--max-time", "1", "http://"+targetIP+":8080/"); err == nil {
		t.Fatal("client bypassed fail-closed barrier")
	}
	must("exec", gateway, "/usr/local/bin/local-vpn-kde", "routing")
	must("exec", gateway, "/usr/local/bin/local-vpn-kde", "health")
	if response := must("exec", client, "curl", "--noproxy", "*", "--fail", "--silent", "--max-time", "10", "http://"+targetIP+":8080/"); response != payload {
		t.Fatal("client traffic did not recover after barrier removal")
	}
	if ipv6After := must("exec", gateway, "ip6tables", "-S", "OVPN_IPV6_BLOCK"); ipv6After != ipv6Before {
		t.Fatal("runtime restarts accumulated IPv6 block rules")
	}
	t.Log("PASS: Go runtime, native PKI/configs, OpenVPN handshake, TUN data path, intercepted DNS, restart acknowledgement, fail-closed barrier, isolated Docker network")
}
