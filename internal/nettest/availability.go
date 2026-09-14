package nettest

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// CheckAvailability always uses the isolated node's SOCKS proxy, including DNS
// and redirects. A failed proxy must never fall back to direct host access.
func CheckAvailability(ctx context.Context, socksAddr, target string, timeout time.Duration) error {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return fmt.Errorf("invalid availability URL")
	}
	transport := &http.Transport{Proxy: http.ProxyURL(&url.URL{Scheme: "socks5", Host: socksAddr}), TLSHandshakeTimeout: timeout, ResponseHeaderTimeout: timeout}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || req.URL.Scheme != "https" || req.URL.User != nil {
			return fmt.Errorf("redirect rejected")
		}
		return nil
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "vibe-vpn/1")
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("site returned HTTP %d", response.StatusCode)
	}
	// Headers prove an HTTPS response; keep body traffic small, never benchmark it.
	_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 1024))
	return err
}
