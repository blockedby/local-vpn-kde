package subscription

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func URLList(s string) []string {
	s = strings.ReplaceAll(s, "\r", "\n")
	out := []string{}
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		out = append(out, l)
	}
	return out
}

func FetchMany(rawURLs []string, timeout time.Duration) ([]string, []error) {
	out := []string{}
	errs := []error{}
	for i, rawURL := range rawURLs {
		links, err := fetchWithTransientRetry(rawURL, timeout)
		if err != nil {
			errs = append(errs, fmt.Errorf("subscription %d fetch failed: %s", i+1, safeFetchError(err)))
			continue
		}
		out = append(out, links...)
	}
	return out, errs
}

func fetchWithTransientRetry(rawURL string, timeout time.Duration) ([]string, error) {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		var links []string
		links, err = Fetch(rawURL, timeout)
		if err == nil {
			return links, nil
		}
		var urlErr *url.Error
		if !errors.As(err, &urlErr) || attempt == 2 {
			return nil, err
		}
		time.Sleep(time.Duration(attempt+1) * 250 * time.Millisecond)
	}
	return nil, err
}

func safeFetchError(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err.Error()
	}
	return err.Error()
}

func Fetch(rawURL string, timeout time.Duration) ([]string, error) {
	cl := http.Client{Timeout: timeout}
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "vibe-vpn/1")
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("subscription fetch failed: HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, err
	}
	return Parse(string(b))
}
func Parse(s string) ([]string, error) {
	txt := strings.TrimSpace(s)
	if !strings.Contains(txt, "vless://") {
		if decoded, ok := decodeBase64(txt); ok {
			txt = decoded
		}
	}
	txt = strings.ReplaceAll(txt, "\r", "\n")
	out := []string{}
	for _, l := range strings.Split(txt, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "vless://") {
			out = append(out, l)
		}
	}
	return out, nil
}

func decodeBase64(s string) (string, bool) {
	raw := strings.Join(strings.Fields(s), "")
	if raw == "" {
		return "", false
	}
	padded := raw + strings.Repeat("=", (4-len(raw)%4)%4)
	encodings := []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding}
	for _, enc := range encodings {
		input := raw
		if enc == base64.StdEncoding || enc == base64.URLEncoding {
			input = padded
		}
		if b, err := enc.DecodeString(input); err == nil {
			return string(b), true
		}
	}
	return "", false
}
