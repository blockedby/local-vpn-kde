// Package geoip maps public IP addresses to countries using embedded data.
// Lookups are entirely offline and do not resolve hostnames.
package geoip

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/binary"
	"io"
	"net/netip"
	"sort"
	"sync"
)

//go:embed country.bin.gz
var compressed []byte
var once sync.Once
var v4, v6 []byte
var excluded = func() []netip.Prefix {
	// IANA special-purpose ranges that must not be associated with a country.
	values := []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4", "::/128", "::1/128", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20", "5f00::/16", "fc00::/7", "fe80::/10", "ff00::/8"}
	result := make([]netip.Prefix, 0, len(values))
	for _, s := range values {
		result = append(result, netip.MustParsePrefix(s))
	}
	return result
}()

func initData() {
	z, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return
	}
	defer z.Close()
	raw, err := io.ReadAll(io.LimitReader(z, 64<<20))
	if err != nil || len(raw) < 12 || string(raw[:4]) != "GEO1" {
		return
	}
	n4, n6 := int(binary.BigEndian.Uint32(raw[4:8])), int(binary.BigEndian.Uint32(raw[8:12]))
	size4, size6 := n4*10, n6*34
	if size4 < 0 || size6 < 0 || size4 > len(raw)-12 || size6 != len(raw)-12-size4 {
		return
	}
	v4 = raw[12 : 12+size4]
	v6 = raw[12+size4:]
}

// Lookup returns an ISO 3166-1 alpha-2 code for a public address, if known.
func Lookup(addr netip.Addr) (string, bool) {
	addr = addr.Unmap()
	if !addr.IsValid() || !addr.IsGlobalUnicast() {
		return "", false
	}
	for _, p := range excluded {
		if p.Contains(addr) {
			return "", false
		}
	}
	once.Do(initData)
	width := 16
	data := v6
	var key []byte
	if addr.Is4() {
		width = 4
		data = v4
		a := addr.As4()
		key = a[:]
	} else {
		a := addr.As16()
		key = a[:]
	}
	record := 2*width + 2
	i := sort.Search(len(data)/record, func(i int) bool { return bytes.Compare(data[i*record+width:i*record+2*width], key) >= 0 })
	if i == len(data)/record {
		return "", false
	}
	r := data[i*record : (i+1)*record]
	if bytes.Compare(r[:width], key) > 0 {
		return "", false
	}
	return string(r[2*width:]), true
}
