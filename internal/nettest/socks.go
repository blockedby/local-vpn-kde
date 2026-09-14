package nettest

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Result struct {
	Bytes   int64   `json:"bytes"`
	Seconds float64 `json:"seconds"`
	Mbps    float64 `json:"mbps"`
}

func Download(socksAddr, testURL string, limit int64, timeout time.Duration) (Result, error) {
	if limit <= 0 {
		return Result{}, fmt.Errorf("download limit must be positive")
	}
	return download(socksAddr, testURL, limit, 0, timeout)
}

func DownloadFor(socksAddr, testURL string, duration, timeout time.Duration) (Result, error) {
	if duration <= 0 {
		return Result{}, fmt.Errorf("download duration must be positive")
	}
	start := time.Now()
	deadline := start.Add(duration)
	var total int64
	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		r, err := download(socksAddr, testURL, 0, remaining, timeout)
		if err != nil {
			if total > 0 {
				break
			}
			return Result{}, err
		}
		total += r.Bytes
		if r.Bytes == 0 {
			break
		}
	}
	sec := time.Since(start).Seconds()
	if total <= 0 {
		return Result{}, fmt.Errorf("download read no body bytes")
	}
	return Result{total, sec, float64(total) * 8 / sec / 1e6}, nil
}

func download(socksAddr, testURL string, limit int64, duration, timeout time.Duration) (Result, error) {
	u, err := url.Parse(testURL)
	if err != nil {
		return Result{}, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return Result{}, fmt.Errorf("unsupported test URL scheme %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return Result{}, fmt.Errorf("test URL missing host")
	}
	if len(host) > 255 {
		return Result{}, fmt.Errorf("test URL host too long for SOCKS5 domain request")
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	d := net.Dialer{Timeout: timeout}
	raw, err := d.Dial("tcp", socksAddr)
	if err != nil {
		return Result{}, err
	}
	defer raw.Close()
	if err := raw.SetDeadline(time.Now().Add(timeout)); err != nil {
		return Result{}, err
	}
	if _, err := raw.Write([]byte{5, 1, 0}); err != nil {
		return Result{}, err
	}
	gr := make([]byte, 2)
	if _, err := io.ReadFull(raw, gr); err != nil || gr[1] != 0 {
		return Result{}, fmt.Errorf("socks greeting")
	}
	hb := []byte(host)
	p, err := strconv.Atoi(port)
	if err != nil || p <= 0 || p > 65535 {
		return Result{}, fmt.Errorf("invalid test URL port %q", port)
	}
	req := append([]byte{5, 1, 0, 3, byte(len(hb))}, hb...)
	req = append(req, byte(p>>8), byte(p))
	if _, err := raw.Write(req); err != nil {
		return Result{}, err
	}
	if err := readSocksReply(raw); err != nil {
		return Result{}, err
	}
	conn := raw
	if u.Scheme == "https" {
		tlsconn := tls.Client(raw, &tls.Config{ServerName: host})
		if err := tlsconn.Handshake(); err != nil {
			return Result{}, err
		}
		conn = tlsconn
	}
	t := time.Now()
	if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: vibe-vpn/1\r\nConnection: close\r\n\r\n", path, u.Host); err != nil {
		return Result{}, err
	}
	if duration > 0 {
		_ = conn.SetDeadline(t.Add(duration))
	}
	buf := make([]byte, 32768)
	var n int64
	header := []byte{}
	haveHeader := false
	for limit <= 0 || n < limit {
		c, err := conn.Read(buf)
		if c > 0 {
			data := buf[:c]
			if !haveHeader {
				header = append(header, data...)
				if len(header) > 64<<10 {
					return Result{}, fmt.Errorf("http response headers too large")
				}
				if i := bytes.Index(header, []byte("\r\n\r\n")); i >= 0 {
					if err := checkHTTPStatus(header[:i]); err != nil {
						return Result{}, err
					}
					haveHeader = true
					n += int64(len(header) - i - 4)
				}
			} else {
				n += int64(c)
			}
		}
		if err != nil {
			break
		}
	}
	if !haveHeader {
		return Result{}, fmt.Errorf("http response missing complete headers")
	}
	sec := time.Since(t).Seconds()
	if sec <= 0 {
		return Result{}, fmt.Errorf("download completed too quickly to measure")
	}
	return Result{n, sec, float64(n) * 8 / sec / 1e6}, nil
}

type HTTPResult struct {
	StatusCode int
	Body       string
	Seconds    float64
}

func Get(socksAddr, targetURL string, maxBody int64, timeout time.Duration) (HTTPResult, error) {
	if maxBody <= 0 {
		return HTTPResult{}, fmt.Errorf("max body must be positive")
	}
	u, err := url.Parse(targetURL)
	if err != nil {
		return HTTPResult{}, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return HTTPResult{}, fmt.Errorf("unsupported URL scheme %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" || len(host) > 255 {
		return HTTPResult{}, fmt.Errorf("invalid URL host")
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	d := net.Dialer{Timeout: timeout}
	raw, err := d.Dial("tcp", socksAddr)
	if err != nil {
		return HTTPResult{}, err
	}
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(timeout))
	start := time.Now()
	if _, err := raw.Write([]byte{5, 1, 0}); err != nil {
		return HTTPResult{}, err
	}
	gr := make([]byte, 2)
	if _, err := io.ReadFull(raw, gr); err != nil || gr[1] != 0 {
		return HTTPResult{}, fmt.Errorf("socks greeting")
	}
	hb := []byte(host)
	p, err := strconv.Atoi(port)
	if err != nil || p <= 0 || p > 65535 {
		return HTTPResult{}, fmt.Errorf("invalid URL port %q", port)
	}
	req := append([]byte{5, 1, 0, 3, byte(len(hb))}, hb...)
	req = append(req, byte(p>>8), byte(p))
	if _, err := raw.Write(req); err != nil {
		return HTTPResult{}, err
	}
	if err := readSocksReply(raw); err != nil {
		return HTTPResult{}, err
	}
	conn := raw
	if u.Scheme == "https" {
		tlsconn := tls.Client(raw, &tls.Config{ServerName: host})
		if err := tlsconn.Handshake(); err != nil {
			return HTTPResult{}, err
		}
		conn = tlsconn
	}
	if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: vibe-vpn/1\r\nConnection: close\r\n\r\n", path, u.Host); err != nil {
		return HTTPResult{}, err
	}
	buf := make([]byte, 8192)
	header := []byte{}
	body := []byte{}
	haveHeader := false
	status := 0
	for int64(len(body)) < maxBody {
		c, err := conn.Read(buf)
		if c > 0 {
			data := buf[:c]
			if !haveHeader {
				header = append(header, data...)
				if len(header) > 64<<10 {
					return HTTPResult{}, fmt.Errorf("http response headers too large")
				}
				if i := bytes.Index(header, []byte("\r\n\r\n")); i >= 0 {
					line := string(header[:i])
					if j := strings.Index(line, "\r\n"); j >= 0 {
						line = line[:j]
					}
					fields := strings.Fields(line)
					if len(fields) < 2 {
						return HTTPResult{}, fmt.Errorf("invalid http response status %q", line)
					}
					status, _ = strconv.Atoi(fields[1])
					if status < 200 || status >= 300 {
						return HTTPResult{}, fmt.Errorf("http response status %d", status)
					}
					haveHeader = true
					body = append(body, header[i+4:]...)
				}
			} else {
				body = append(body, data...)
			}
		}
		if err != nil {
			break
		}
	}
	if !haveHeader {
		return HTTPResult{}, fmt.Errorf("http response missing complete headers")
	}
	if int64(len(body)) > maxBody {
		body = body[:maxBody]
	}
	return HTTPResult{StatusCode: status, Body: string(body), Seconds: time.Since(start).Seconds()}, nil
}

func checkHTTPStatus(header []byte) error {
	line := string(header)
	if i := strings.Index(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	fields := strings.Fields(line)
	if len(fields) < 2 || !strings.HasPrefix(fields[0], "HTTP/") {
		return fmt.Errorf("invalid http response status %q", line)
	}
	code, err := strconv.Atoi(fields[1])
	if err != nil {
		return fmt.Errorf("invalid http response status %q", line)
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("http response status %d", code)
	}
	return nil
}

func readSocksReply(r io.Reader) error {
	h := make([]byte, 4)
	if _, err := io.ReadFull(r, h); err != nil {
		return err
	}
	if h[0] != 5 || h[1] != 0 {
		return fmt.Errorf("socks connect reply %v", h)
	}
	var rest int
	switch h[3] {
	case 1:
		rest = 4 + 2
	case 3:
		lb := make([]byte, 1)
		if _, err := io.ReadFull(r, lb); err != nil {
			return err
		}
		rest = int(lb[0]) + 2
	case 4:
		rest = 16 + 2
	default:
		return fmt.Errorf("socks connect unknown address type %d", h[3])
	}
	_, err := io.CopyN(io.Discard, r, int64(rest))
	return err
}
