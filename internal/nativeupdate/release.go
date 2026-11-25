package nativeupdate

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const Repository = "blockedby/local-vpn-kde"
const DefaultAPIBase = "https://api.github.com"
const DefaultDownloadBase = "https://github.com"

const maxReleaseBody = 1 << 20
const maxChecksumsBody = 64 << 10
const maxAssetSize = 512 << 20

type Version struct{ Major, Minor, Patch int }

func ParseVersion(s string) (Version, error) {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("invalid version %q", s)
	}
	var nums [3]int
	for i, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return Version{}, fmt.Errorf("invalid version %q", s)
		}
		for _, ch := range part {
			if ch < '0' || ch > '9' {
				return Version{}, fmt.Errorf("invalid version %q", s)
			}
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return Version{}, fmt.Errorf("invalid version %q: %w", s, err)
		}
		nums[i] = n
	}
	return Version{nums[0], nums[1], nums[2]}, nil
}

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

func (v Version) Compare(o Version) int {
	for i, n := range [...]int{v.Major, v.Minor, v.Patch} {
		other := [...]int{o.Major, o.Minor, o.Patch}[i]
		if n < other {
			return -1
		}
		if n > other {
			return 1
		}
	}
	return 0
}

type Release struct {
	Version Version
	Tag     string
	Assets  map[string]int64
}

type Client struct {
	HTTP         *http.Client
	APIBase      string
	DownloadBase string
	UserAgent    string
}

func NewClient(userAgent string) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.TLSHandshakeTimeout = 15 * time.Second
	return &Client{HTTP: &http.Client{Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 5 {
			return fmt.Errorf("too many redirects")
		}
		host := strings.ToLower(req.URL.Hostname())
		allowed := req.URL.Scheme == "https" && (host == "github.com" || host == "api.github.com" || strings.HasSuffix(host, ".githubusercontent.com"))
		// Same-origin redirects also permit overridden local httptest endpoints.
		original := via[0].URL
		sameOrigin := req.URL.Scheme == original.Scheme && strings.EqualFold(req.URL.Host, original.Host)
		if !allowed && !sameOrigin {
			return fmt.Errorf("redirect to %s is not allowed", req.URL.Host)
		}
		return nil
	}}, APIBase: DefaultAPIBase, DownloadBase: DefaultDownloadBase, UserAgent: userAgent}
}

func (c *Client) get(ctx context.Context, address string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", c.UserAgent)
	client := c.HTTP
	if client == nil {
		client = NewClient(c.UserAgent).HTTP
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP status %d", resp.StatusCode)
	}
	return resp, nil
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return data, nil
}

func (c *Client) Latest(ctx context.Context) (Release, error) {
	release, err := c.latest(ctx)
	if err != nil {
		return Release{}, fmt.Errorf("check latest release: %w", err)
	}
	return release, nil
}

func (c *Client) latest(ctx context.Context) (Release, error) {
	base := c.APIBase
	if base == "" {
		base = DefaultAPIBase
	}
	resp, err := c.get(ctx, strings.TrimRight(base, "/")+"/repos/"+Repository+"/releases/latest")
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	body, err := readLimited(resp.Body, maxReleaseBody)
	if err != nil {
		return Release{}, err
	}
	var data struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		Assets     []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return Release{}, fmt.Errorf("decode release: %w", err)
	}
	if data.Draft || data.Prerelease {
		return Release{}, fmt.Errorf("release is a draft or prerelease")
	}
	if !strings.HasPrefix(data.Tag, "v") {
		return Release{}, fmt.Errorf("invalid release tag %q", data.Tag)
	}
	version, err := ParseVersion(strings.TrimPrefix(data.Tag, "v"))
	if err != nil {
		return Release{}, fmt.Errorf("invalid release tag %q: %w", data.Tag, err)
	}
	release := Release{Version: version, Tag: data.Tag, Assets: make(map[string]int64)}
	for _, asset := range data.Assets {
		if _, ok := release.Assets[asset.Name]; ok {
			return Release{}, fmt.Errorf("duplicate asset %q", asset.Name)
		}
		if asset.Size <= 0 {
			return Release{}, fmt.Errorf("invalid size for asset %q", asset.Name)
		}
		release.Assets[asset.Name] = asset.Size
	}
	return release, nil
}

func (c *Client) assetResponse(ctx context.Context, r Release, name string) (*http.Response, error) {
	if _, ok := r.Assets[name]; !ok {
		return nil, fmt.Errorf("asset is not in release")
	}
	// Escape each segment so release metadata cannot change the constructed URL path.
	base := c.DownloadBase
	if base == "" {
		base = DefaultDownloadBase
	}
	address := strings.TrimRight(base, "/") + "/" + Repository + "/releases/download/" + url.PathEscape(r.Tag) + "/" + url.PathEscape(name)
	return c.get(ctx, address)
}

var checksumLine = regexp.MustCompile(`^([0-9a-fA-F]{64}) (?: |\*)(.+)$`)

func (c *Client) Checksums(ctx context.Context, r Release) (map[string][32]byte, error) {
	sums, err := c.checksums(ctx, r)
	if err != nil {
		return nil, fmt.Errorf("download SHA256SUMS: %w", err)
	}
	return sums, nil
}

func (c *Client) checksums(ctx context.Context, r Release) (map[string][32]byte, error) {
	resp, err := c.assetResponse(ctx, r, "SHA256SUMS")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := readLimited(resp.Body, maxChecksumsBody)
	if err != nil {
		return nil, err
	}
	sums := make(map[string][32]byte)
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	scanner.Buffer(make([]byte, 1024), maxChecksumsBody+1)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		match := checksumLine.FindStringSubmatch(line)
		if match == nil {
			return nil, fmt.Errorf("malformed checksum line")
		}
		if _, ok := sums[match[2]]; ok {
			return nil, fmt.Errorf("duplicate checksum for %q", match[2])
		}
		decoded, err := hex.DecodeString(match[1])
		if err != nil {
			return nil, fmt.Errorf("invalid checksum: %w", err)
		}
		var sum [32]byte
		copy(sum[:], decoded)
		sums[match[2]] = sum
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return sums, nil
}

func (c *Client) Download(ctx context.Context, r Release, asset string, sums map[string][32]byte, dst io.Writer) error {
	if err := c.download(ctx, r, asset, sums, dst); err != nil {
		return fmt.Errorf("download %s: %w", asset, err)
	}
	return nil
}

func (c *Client) download(ctx context.Context, r Release, asset string, sums map[string][32]byte, dst io.Writer) error {
	size, ok := r.Assets[asset]
	if !ok {
		return fmt.Errorf("asset is not in release")
	}
	if size <= 0 || size > maxAssetSize {
		return fmt.Errorf("invalid declared asset size %d", size)
	}
	expected, ok := sums[asset]
	if !ok {
		return fmt.Errorf("checksum is missing")
	}
	resp, err := c.assetResponse(ctx, r, asset)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	hash := sha256.New()
	written, err := io.CopyN(io.MultiWriter(dst, hash), resp.Body, size)
	if err != nil {
		return fmt.Errorf("asset ended after %d of %d bytes: %w", written, size, err)
	}
	var extra [1]byte
	n, err := io.ReadFull(resp.Body, extra[:])
	if n > 0 {
		return fmt.Errorf("asset exceeds declared size %d", size)
	}
	if err != io.EOF {
		return fmt.Errorf("read asset: %w", err)
	}
	var actual [32]byte
	copy(actual[:], hash.Sum(nil))
	if actual != expected {
		return fmt.Errorf("checksum mismatch for %s", asset)
	}
	return nil
}

func AssetName(manager string, v Version) (string, error) {
	switch manager {
	case "pacman":
		return fmt.Sprintf("local-vpn-kde-native-%s-1-x86_64.pkg.tar.zst", v), nil
	case "dpkg":
		return fmt.Sprintf("local-vpn-kde-native_%s_amd64.deb", v), nil
	default:
		return "", fmt.Errorf("unsupported package manager %q", manager)
	}
}
