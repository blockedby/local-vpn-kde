// Command geoip-generator refreshes the embedded, offline server-country data.
// Run manually from the repository root: go run ./scripts/geoip-generator
package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"
)

const base = "https://github.com/sapics/ip-location-db/releases/download/"

type entry struct {
	start, end *big.Int
	code       string
}
type source struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}
type manifest struct {
	Source  string   `json:"source"`
	Date    string   `json:"date_utc"`
	Licence string   `json:"licence"`
	Inputs  []source `json:"inputs"`
}

func fetch(url string) ([]byte, error) {
	c := &http.Client{Timeout: 2 * time.Minute}
	r, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", url, r.Status)
	}
	return io.ReadAll(io.LimitReader(r.Body, 100<<20))
}
func load(family string) ([]entry, source, error) {
	name := "server-country-" + family + "-num.csv"
	url := base + "latest/" + name
	data, err := fetch(url)
	if err != nil {
		return nil, source{}, err
	}
	checksum, err := fetch(base + "checksum/" + name + ".sha256")
	if err != nil {
		return nil, source{}, err
	}
	fields := strings.Fields(string(checksum))
	if len(fields) == 0 {
		return nil, source{}, fmt.Errorf("empty checksum")
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	if digest != fields[0] {
		return nil, source{}, fmt.Errorf("checksum mismatch for %s", name)
	}
	rd := csv.NewReader(bytes.NewReader(data))
	var ranges []entry
	limit := 128
	if family == "ipv4" {
		limit = 32
	}
	for {
		row, e := rd.Read()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, source{}, e
		}
		if len(row) != 3 {
			return nil, source{}, fmt.Errorf("bad CSV row")
		}
		start, ok := new(big.Int).SetString(row[0], 10)
		if !ok || start.Sign() < 0 || start.BitLen() > limit {
			return nil, source{}, fmt.Errorf("bad range start")
		}
		end, ok := new(big.Int).SetString(row[1], 10)
		if !ok || end.Sign() < 0 || end.BitLen() > limit || start.Cmp(end) > 0 {
			return nil, source{}, fmt.Errorf("bad range end")
		}
		code := row[2]
		if len(code) != 2 || code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' {
			return nil, source{}, fmt.Errorf("bad country code %q", code)
		}
		if code == "XK" {
			continue
		} // Kosovo is not an ISO 3166-1 alpha-2 code.
		if len(ranges) > 0 {
			prev := &ranges[len(ranges)-1]
			if start.Cmp(prev.end) <= 0 {
				return nil, source{}, fmt.Errorf("overlapping ranges")
			}
			next := new(big.Int).Add(prev.end, big.NewInt(1))
			if prev.code == code && start.Cmp(next) == 0 {
				prev.end = end
				continue
			}
		}
		ranges = append(ranges, entry{start, end, code})
	}
	return ranges, source{url, digest}, nil
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	v4, s4, err := load("ipv4")
	if err != nil {
		return err
	}
	v6, s6, err := load("ipv6")
	if err != nil {
		return err
	}
	var raw bytes.Buffer
	raw.WriteString("GEO1")
	binary.Write(&raw, binary.BigEndian, uint32(len(v4)))
	binary.Write(&raw, binary.BigEndian, uint32(len(v6)))
	for family, ranges := range [][]entry{v4, v6} {
		width := 16
		if family == 0 {
			width = 4
		}
		for _, r := range ranges {
			raw.Write(r.start.FillBytes(make([]byte, width)))
			raw.Write(r.end.FillBytes(make([]byte, width)))
			raw.WriteString(r.code)
		}
	}
	var packed bytes.Buffer
	z, _ := gzip.NewWriterLevel(&packed, gzip.BestCompression)
	if _, err = z.Write(raw.Bytes()); err != nil {
		return err
	}
	if err = z.Close(); err != nil {
		return err
	}
	if packed.Len() > 6_000_000 {
		return fmt.Errorf("database exceeds 6 MB: %d", packed.Len())
	}
	m := manifest{"sapics/ip-location-db server-country", time.Now().UTC().Format("2006-01-02"), "PDDL 1.0 (Public Domain Dedication and License)", []source{s4, s6}}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile("internal/geoip/country.bin.gz", packed.Bytes(), 0644); err != nil {
		return err
	}
	if err = os.WriteFile("internal/geoip/manifest.json", append(body, '\n'), 0644); err != nil {
		return err
	}
	fmt.Printf("%d IPv4 ranges, %d IPv6 ranges, %d bytes\n", len(v4), len(v6), packed.Len())
	return nil
}
