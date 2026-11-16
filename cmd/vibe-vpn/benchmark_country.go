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
	observed  bool
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
			c.observed = true
			c.ambiguous = true
		}
	}
	return len(p), nil
}

func (c *benchmarkCountryCollector) consume(line []byte) {
	marker := " dns: lookup succeed for " + c.host
	i := bytes.Index(line, []byte(marker))
	if i < 0 || !bytes.HasPrefix(line, []byte("DEBUG")) {
		return
	}
	rest := line[i+len(marker):]
	if len(rest) > 0 && (rest[0] >= 'a' && rest[0] <= 'z' || rest[0] >= 'A' && rest[0] <= 'Z' || rest[0] >= '0' && rest[0] <= '9' || rest[0] == '.' || rest[0] == '-' || rest[0] == '_') {
		return
	}
	c.observed = true
	if !bytes.HasPrefix(rest, []byte(": ")) {
		c.ambiguous = true
		return
	}
	fields := bytes.Fields(rest[2:])
	if len(fields) == 0 {
		c.ambiguous = true
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
	code, _ := c.countryObservation()
	return code
}

func (c *benchmarkCountryCollector) countryObservation() (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.line) > 0 {
		return "", true
	}
	if c.ambiguous {
		return "", c.observed
	}
	return c.code, c.observed
}

func measureProxyCountry(ctx context.Context, host string, measure func(context.Context) (nettest.Result, error)) (nettest.Result, error) {
	collector := newBenchmarkCountryCollector(host)
	if collector != nil {
		ctx = context.WithValue(ctx, benchmarkCountryKey{}, collector)
	}
	result, err := measure(ctx)
	result.Country, result.CountryObserved = collector.countryObservation()
	return result, err
}
