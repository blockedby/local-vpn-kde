package localvpn

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/blockedby/local-vpn-kde/internal/singbox"
	"github.com/blockedby/local-vpn-kde/internal/state"
	"golang.org/x/sys/unix"
)

func probeDNS(ctx context.Context, host, address string) bool {
	transport := &http.Transport{Proxy: http.ProxyURL(&url.URL{Scheme: "socks5", Host: "127.0.0.1:2080"}), TLSClientConfig: &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 4 * time.Second, ResponseHeaderTimeout: 4 * time.Second}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+address+"/dns-query?name=example.com&type=A", nil)
	if err != nil {
		return false
	}
	req.Host = host
	req.Header.Set("Accept", "application/dns-json")
	response, err := client.Do(req)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return false
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 65537))
	if err != nil || len(data) > 65536 {
		return false
	}
	var payload struct {
		Status *int `json:"Status"`
	}
	return json.Unmarshal(data, &payload) == nil && payload.Status != nil && *payload.Status == 0
}
func (c ContainerConfig) watchDNS(ctx context.Context, output io.Writer) {
	interval := 15 * time.Second
	if raw := os.Getenv("VPNKIT_DNS_FAILOVER_INTERVAL_SECONDS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 3600 {
			fmt.Fprintln(output, "dns_failover=invalid_interval")
			return
		}
		interval = time.Duration(n) * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		target := ""
		if probeDNS(ctx, "cloudflare-dns.com", "1.1.1.1") {
			target = "remote-dns"
		} else if probeDNS(ctx, "dns.google", "8.8.8.8") {
			target = "remote-dns-fallback"
		}
		if target != "" {
			if err := c.SwitchDNS(ctx, target); err != nil && ctx.Err() == nil {
				fmt.Fprintln(output, "dns_failover_apply=skipped")
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (c ContainerConfig) SwitchDNS(ctx context.Context, target string) error {
	if target != "remote-dns" && target != "remote-dns-fallback" {
		return errors.New("invalid DNS target")
	}
	stateDir := os.Getenv("VPNKIT_DNS_FAILOVER_STATE_DIR")
	if stateDir == "" {
		stateDir = "/var/lib/vibe-vpn"
	}
	lockCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	lock, err := state.AcquireLock(lockCtx, stateDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	previous, err := runtimeRead(c.Runtime)
	if err != nil {
		return err
	}
	var config map[string]any
	if json.Unmarshal(previous, &config) != nil {
		return errors.New("invalid runtime JSON")
	}
	dns, ok := config["dns"].(map[string]any)
	if !ok {
		return nil
	}
	servers, ok := dns["servers"].([]any)
	if !ok {
		return nil
	}
	found := map[string]bool{}
	for _, value := range servers {
		if server, ok := value.(map[string]any); ok {
			if tag, ok := server["tag"].(string); ok {
				found[tag] = true
			}
		}
	}
	if !found["remote-dns"] || !found["remote-dns-fallback"] || dns["final"] == target {
		return nil
	}
	if dns["final"] != "remote-dns" && dns["final"] != "remote-dns-fallback" {
		return nil
	}
	dns["final"] = target
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	dir, err := directory(filepath.Dir(c.Runtime), false, true)
	if err != nil {
		return err
	}
	defer unix.Close(dir)
	candidate, err := stagePrivate(dir, append(data, '\n'), 0600)
	if err != nil {
		return err
	}
	defer candidate.close()
	candidatePath := fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), candidate.file.Fd())
	if _, err = runtimeCommand(ctx, c.Binary, "check", "-c", candidatePath); err != nil {
		return errors.New("DNS candidate rejected")
	}
	if !candidate.matches(candidate.name) {
		return errors.New("DNS candidate changed")
	}
	if err = unix.Renameat(dir, candidate.name, dir, filepath.Base(c.Runtime)); err != nil {
		return err
	}
	if err = unix.Fsync(dir); err != nil {
		_ = runtimeWrite(c.Runtime, previous)
		return err
	}
	restart := singbox.RestartConfig{Mode: singbox.RestartModeRequestFile, RequestFile: c.Request, AckGenerationFile: c.Generation, AckFile: c.Ack, AckTimeout: 60 * time.Second}
	if err = singbox.RestartWithAckContext(ctx, restart); err != nil {
		if !candidate.matches(filepath.Base(c.Runtime)) {
			return errors.New("DNS rollback identity changed")
		}
		if restoreErr := runtimeWrite(c.Runtime, previous); restoreErr != nil {
			return errors.New("DNS rollback failed")
		}
		// The parent may be shutting down; never keep it waiting for an ack that
		// its canceled supervisor can no longer publish.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rollback, done := context.WithTimeout(context.Background(), 65*time.Second)
		defer done()
		if restoreErr := singbox.RestartWithAckContext(rollback, restart); restoreErr != nil {
			return errors.New("DNS rollback acknowledgement failed")
		}
		return errors.New("DNS restart failed; prior configuration restored")
	}
	return nil
}
