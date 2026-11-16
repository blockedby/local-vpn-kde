package main

import (
	"bytes"
	"context"
	"net/netip"
	"sync"

	"github.com/blockedby/local-vpn-kde/internal/geoip"
	"github.com/blockedby/local-vpn-kde/internal/nettest"
)

type benchmarkCountryKey struct{}

// sing-box logs its own DNS lookup results at debug level. Its resolver may
// race several addresses and does not log the winner, so only accept a result
// when every address has the same known country. The stream is private and
// discarded immediately after parsing; only the country is retained.
type benchmarkCountryCollector struct {
	mu        sync.Mutex
	host      string
	line      []byte
	overflow  bool
	code      string
	ambiguous bool
}

func newBenchmarkCountryCollector(host string) *benchmarkCountryCollector {
	if host == "" || len(host) > 253 {
		return nil
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return nil
	}
	for _, c := range host {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_') {
			return nil
		}
	}
	return &benchmarkCountryCollector{host: host}
}

func (c *benchmarkCountryCollector) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, b := range p {
		if b == '\n' {
			if !c.overflow {
				c.consume(c.line)
			}
			clear(c.line)
			c.line = c.line[:0]
			c.overflow = false
		} else if len(c.line) < 16384 && !c.overflow {
			c.line = append(c.line, b)
		} else {
			c.overflow = true
		}
	}
	return len(p), nil
}

func (c *benchmarkCountryCollector) consume(line []byte) {
	marker := " dns: lookup succeed for " + c.host + ": "
	i := bytes.Index(line, []byte(marker))
	if i < 0 || !bytes.HasPrefix(line, []byte("DEBUG")) {
		return
	}
	fields := bytes.Fields(line[i+len(marker):])
	if len(fields) == 0 {
		return
	}
	for _, field := range fields {
		addr, err := netip.ParseAddr(string(field))
		code, ok := geoip.Lookup(addr)
		if err != nil || !ok || c.code != "" && c.code != code {
			c.ambiguous = true
			return
		}
		c.code = code
	}
}

func (c *benchmarkCountryCollector) country() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ambiguous {
		return ""
	}
	return c.code
}

func measureProxyCountry(ctx context.Context, host string, measure func(context.Context) (nettest.Result, error)) (nettest.Result, error) {
	collector := newBenchmarkCountryCollector(host)
	if collector != nil {
		ctx = context.WithValue(ctx, benchmarkCountryKey{}, collector)
	}
	result, err := measure(ctx)
	if err == nil {
		result.Country = collector.country()
	}
	return result, err
}
