package nativeupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseVersion(t *testing.T) {
	for _, s := range []string{"01.2.3", "1.2", "v1.2.3", "1.2.3-rc1", "1.2.03", "1..3", "+1.2.3", "999999999999999999999999.2.3"} {
		t.Run(s, func(t *testing.T) {
			if _, err := ParseVersion(s); err == nil {
				t.Fatal("expected error")
			}
		})
	}
	for _, s := range []string{"0.0.0", "1.2.3", "123.45.6"} {
		v, err := ParseVersion(s)
		if err != nil || v.String() != s {
			t.Fatalf("ParseVersion(%q) = %v, %v", s, v, err)
		}
	}
}

func TestCompare(t *testing.T) {
	for _, tc := range []struct {
		a, b Version
		want int
	}{
		{Version{1, 0, 0}, Version{2, 0, 0}, -1},
		{Version{2, 0, 0}, Version{1, 99, 99}, 1},
		{Version{1, 2, 3}, Version{1, 2, 3}, 0},
		{Version{1, 2, 4}, Version{1, 2, 3}, 1},
		{Version{1, 1, 9}, Version{1, 2, 0}, -1},
	} {
		if got := tc.a.Compare(tc.b); got != tc.want {
			t.Errorf("%v.Compare(%v) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestLatest(t *testing.T) {
	for _, tc := range []struct {
		name, body, errorContains string
		status                    int
	}{
		{"success", `{"tag_name":"v1.2.3","assets":[{"name":"a","size":42}]}`, "", 200},
		{"draft", `{"tag_name":"v1.2.3","draft":true}`, "draft", 200},
		{"prerelease", `{"tag_name":"v1.2.3","prerelease":true}`, "prerelease", 200},
		{"bad tag", `{"tag_name":"v01.2.3"}`, "tag", 200},
		{"duplicate", `{"tag_name":"v1.2.3","assets":[{"name":"a","size":1},{"name":"a","size":2}]}`, "duplicate", 200},
		{"invalid size", `{"tag_name":"v1.2.3","assets":[{"name":"a","size":0}]}`, "size", 200},
		{"oversize", strings.Repeat("x", maxReleaseBody+1), "exceeds", 200},
		{"rate limit", ``, "403", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/"+Repository+"/releases/latest" {
					t.Errorf("path = %s", r.URL.Path)
				}
				if r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("User-Agent") != "test-agent" {
					t.Errorf("unexpected headers: %v", r.Header)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer s.Close()
			c := NewClient("test-agent")
			c.APIBase = s.URL
			got, err := c.Latest(context.Background())
			if tc.errorContains != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errorContains) || !strings.Contains(err.Error(), "check latest release") {
					t.Fatalf("error = %v", err)
				}
				return
			}
			if err != nil || got.Version != (Version{1, 2, 3}) || got.Tag != "v1.2.3" || got.Assets["a"] != 42 {
				t.Fatalf("got %+v, %v", got, err)
			}
		})
	}
}

func TestChecksums(t *testing.T) {
	digest := sha256.Sum256([]byte("payload"))
	hex := fmt.Sprintf("%x", digest)
	for _, tc := range []struct {
		name, body, errorContains string
		missing                   bool
	}{
		{"valid", hex + "  file\n" + strings.ToUpper(hex) + " *binary\n\n", "", false},
		{"malformed", hex + " file\n", "malformed", false},
		{"duplicate", hex + "  file\n" + hex + " *file\n", "duplicate", false},
		{"missing", "", "not in release", true},
		{"oversize", strings.Repeat("x", maxChecksumsBody+1), "exceeds", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/blockedby/local-vpn-kde/releases/download/v1.2.3/SHA256SUMS" {
					t.Errorf("path = %s", r.URL.Path)
				}
				fmt.Fprint(w, tc.body)
			}))
			defer s.Close()
			c := NewClient("test")
			c.DownloadBase = s.URL
			r := Release{Tag: "v1.2.3", Assets: map[string]int64{}}
			if !tc.missing {
				r.Assets["SHA256SUMS"] = 1
			}
			got, err := c.Checksums(context.Background(), r)
			if tc.errorContains != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errorContains) {
					t.Fatalf("error = %v", err)
				}
				return
			}
			if err != nil || len(got) != 2 || got["file"] != digest || got["binary"] != digest {
				t.Fatalf("got %v, %v", got, err)
			}
		})
	}
}

func TestDownload(t *testing.T) {
	payload := "payload"
	digest := sha256.Sum256([]byte(payload))
	for _, tc := range []struct {
		name, body, errorContains         string
		size                              int64
		missingAsset, missingSum, badHash bool
	}{
		{"success", payload, "", 7, false, false, false},
		{"wrong hash", payload, "checksum mismatch for file", 7, false, false, true},
		{"short", "short", "ended", 7, false, false, false},
		{"long", "payload extra", "exceeds", 7, false, false, false},
		{"missing asset", payload, "not in release", 7, true, false, false},
		{"missing sum", payload, "checksum is missing", 7, false, true, false},
		{"too large", payload, "size", maxAssetSize + 1, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.URL.EscapedPath() != "/blockedby/local-vpn-kde/releases/download/v1.2.3/file" {
					t.Errorf("path = %s", r.URL.EscapedPath())
				}
				fmt.Fprint(w, tc.body)
			}))
			defer s.Close()
			c := NewClient("test")
			c.DownloadBase = s.URL
			r := Release{Tag: "v1.2.3", Assets: map[string]int64{}}
			if !tc.missingAsset {
				r.Assets["file"] = tc.size
			}
			sums := map[string][32]byte{}
			if !tc.missingSum {
				sums["file"] = digest
			}
			if tc.badHash {
				sums["file"] = [32]byte{}
			}
			var dst bytes.Buffer
			err := c.Download(context.Background(), r, "file", sums, &dst)
			if tc.errorContains != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errorContains) || !strings.Contains(err.Error(), "download file") {
					t.Fatalf("error = %v", err)
				}
				if tc.name == "long" && dst.String() != payload {
					t.Fatalf("wrote extra bytes: %q", dst.String())
				}
				if (tc.missingAsset || tc.missingSum || tc.size > maxAssetSize) && requests != 0 {
					t.Fatal("unexpected request")
				}
				return
			}
			if err != nil || dst.String() != payload {
				t.Fatalf("download = %q, %v", dst.String(), err)
			}
		})
	}
}

func TestRedirects(t *testing.T) {
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("disallowed server reached") }))
	defer second.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, second.URL, http.StatusFound) }))
	defer first.Close()
	c := NewClient("test")
	c.APIBase = first.URL
	if _, err := c.Latest(context.Background()); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("error = %v", err)
	}
}

func TestClientDefaultsAndSameOriginRedirect(t *testing.T) {
	c := NewClient("test")
	if c.HTTP.Timeout != 0 || c.HTTP.Transport.(*http.Transport).ResponseHeaderTimeout != 30*time.Second || c.HTTP.Transport.(*http.Transport).TLSHandshakeTimeout != 15*time.Second {
		t.Fatal("unexpected client timeouts")
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/final" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		fmt.Fprint(w, `{"tag_name":"v0.1.0"}`)
	}))
	defer s.Close()
	c.APIBase = s.URL
	if _, err := c.Latest(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAssetName(t *testing.T) {
	for _, tc := range []struct {
		manager, want string
		bad           bool
	}{
		{"pacman", "local-vpn-kde-native-1.2.3-1-x86_64.pkg.tar.zst", false},
		{"dpkg", "local-vpn-kde-native_1.2.3_amd64.deb", false},
		{"other", "", true},
	} {
		got, err := AssetName(tc.manager, Version{1, 2, 3})
		if got != tc.want || (err != nil) != tc.bad {
			t.Errorf("AssetName(%q) = %q, %v", tc.manager, got, err)
		}
	}
}
