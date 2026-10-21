// This fixture runs only in a disposable container.
package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"github.com/blockedby/local-vpn-kde/internal/sharelink"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func run() error {
	if os.Getenv("VPNKIT_HY2_LAB") != "1" {
		return fmt.Errorf("lab opt-in missing")
	}
	if _, e := os.Stat("/.dockerenv"); e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	dir, e := os.MkdirTemp("", "hy2-fixture-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(dir)
	if source := os.Getenv("VPNKIT_LAB_SOURCE_FILE"); source != "" {
		return realSource(ctx, dir, source)
	}
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		return e
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	cert, e := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if e != nil {
		return e
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	cp, kp := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	os.WriteFile(cp, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert}), 0600)
	os.WriteFile(kp, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0600)
	udp, e := net.ListenPacket("udp", "127.0.0.1:0")
	if e != nil {
		return e
	}
	port := udp.LocalAddr().(*net.UDPAddr).Port
	udp.Close()
	tcp, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		return e
	}
	socksPort := tcp.Addr().(*net.TCPAddr).Port
	tcp.Close()
	responder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "hysteria2-profile-fixture") }))
	defer responder.Close()
	serverCfg := map[string]any{"inbounds": []any{map[string]any{"type": "hysteria2", "listen": "127.0.0.1", "listen_port": port, "users": []any{map[string]any{"name": "fixture", "password": "fixture-pass"}}, "obfs": map[string]any{"type": "salamander", "password": "fixture-mask"}, "tls": map[string]any{"enabled": true, "certificate_path": cp, "key_path": kp}}}, "outbounds": []any{map[string]any{"type": "direct"}}}
	outbound, e := sharelink.Outbound(fmt.Sprintf("hy2://fixture-pass@127.0.0.1:%d/?sni=localhost&insecure=1&obfs=salamander&obfs-password=fixture-mask#fixture", port))
	if e != nil {
		return e
	}
	outbound["tag"] = "proxy"
	clientCfg := map[string]any{"inbounds": []any{map[string]any{"type": "socks", "listen": "127.0.0.1", "listen_port": socksPort}}, "outbounds": []any{outbound}, "route": map[string]any{"final": "proxy"}}
	start := func(name string, cfg any) (*exec.Cmd, error) {
		b, _ := json.Marshal(cfg)
		path := filepath.Join(dir, name+".json")
		os.WriteFile(path, b, 0600)
		check := exec.CommandContext(ctx, "sing-box", "check", "-c", path)
		if b, e := check.CombinedOutput(); e != nil {
			return nil, fmt.Errorf("fixture config: %s %v", b, e)
		}
		cmd := exec.CommandContext(ctx, "sing-box", "run", "-c", path)
		var log bytes.Buffer
		cmd.Stderr = &log
		if e := cmd.Start(); e != nil {
			return nil, e
		}
		return cmd, nil
	}
	server, e := start("server", serverCfg)
	if e != nil {
		return e
	}
	defer func() { server.Process.Kill(); server.Wait() }()
	client, e := start("client", clientCfg)
	if e != nil {
		return e
	}
	defer func() { client.Process.Kill(); client.Wait() }()
	proxy, _ := url.Parse("socks5://127.0.0.1:" + strconv.Itoa(socksPort))
	tr := &http.Transport{Proxy: http.ProxyURL(proxy), DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr, Timeout: 2 * time.Second}
	ok := false
	for i := 0; i < 30; i++ {
		resp, e := hc.Get(responder.URL)
		if e == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(b) == "hysteria2-profile-fixture" {
				ok = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ok {
		return fmt.Errorf("Hysteria2 fixture transfer failed")
	}
	server.Process.Kill()
	server.Wait()
	tr.CloseIdleConnections()
	if resp, e := hc.Get(responder.URL); e == nil {
		resp.Body.Close()
		return fmt.Errorf("unexpected direct fallback")
	}
	fmt.Println("hysteria2_share_link_packet_path=passed; salamander,password,TLS-options; no-direct-fallback=passed")
	return nil
}

// Credentials and sing-box diagnostics stay inside the disposable container.
func realSource(ctx context.Context, dir, source string) error {
	b, e := os.ReadFile(source)
	if e != nil {
		return fmt.Errorf("cannot read private source")
	}
	outbound, e := sharelink.Outbound(strings.TrimSpace(string(b)))
	if e != nil {
		return e
	}
	outbound["tag"] = "proxy"
	cfg := map[string]any{"inbounds": []any{map[string]any{"type": "socks", "listen": "127.0.0.1", "listen_port": 1080}}, "outbounds": []any{outbound}, "route": map[string]any{"final": "proxy"}}
	data, _ := json.Marshal(cfg)
	path := filepath.Join(dir, "client.json")
	if e := os.WriteFile(path, data, 0600); e != nil {
		return e
	}
	cmd := exec.CommandContext(ctx, "sing-box", "run", "-c", path)
	if e := cmd.Start(); e != nil {
		return fmt.Errorf("client start failed")
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	proxy, _ := url.Parse("socks5://127.0.0.1:1080")
	tr := &http.Transport{Proxy: http.ProxyURL(proxy), DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr, Timeout: 8 * time.Second}
	for i := 0; i < 3; i++ {
		time.Sleep(200 * time.Millisecond)
		resp, e := hc.Get("https://www.gstatic.com/generate_204")
		if e == nil {
			resp.Body.Close()
			if resp.StatusCode == 204 {
				fmt.Println("real_source_https_via_proxy=passed; TLS_verification=enabled")
				return nil
			}
		}
	}
	return fmt.Errorf("real source HTTPS probe failed (details withheld)")
}
