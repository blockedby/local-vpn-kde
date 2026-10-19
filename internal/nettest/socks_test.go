package nettest

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCheckHTTPStatus(t *testing.T) {
	if err := checkHTTPStatus([]byte("HTTP/1.1 204 No Content\r\nServer: test")); err != nil {
		t.Fatal(err)
	}
	if err := checkHTTPStatus([]byte("HTTP/1.1 404 Not Found\r\nServer: test")); err == nil {
		t.Fatal("expected non-2xx status to fail")
	}
	if err := checkHTTPStatus([]byte("not-http")); err == nil {
		t.Fatal("expected malformed status to fail")
	}
}

func TestReadSocksReplyConsumesDomainReply(t *testing.T) {
	reply := []byte{5, 0, 0, 3, 3, 'a', 'b', 'c', 0x12, 0x34, 'T', 'L', 'S'}
	r := bytes.NewReader(reply)
	if err := readSocksReply(r); err != nil {
		t.Fatal(err)
	}
	left, _ := r.ReadByte()
	if left != 'T' {
		t.Fatalf("reply parser consumed application byte %q", left)
	}
}

func TestReadSocksReplyRejectsFailure(t *testing.T) {
	if err := readSocksReply(bytes.NewReader([]byte{5, 5, 0, 1})); err == nil {
		t.Fatal("expected error")
	}
}

func TestAvailabilityNeverFallsBackToDirect(t *testing.T) {
	var reached atomic.Bool
	site := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Store(true); w.WriteHeader(200) }))
	defer site.Close()
	// A reachable target with a dead proxy must fail without touching the target.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	err = CheckAvailability(context.Background(), address, site.URL, time.Second)
	if err == nil || reached.Load() {
		t.Fatal("availability bypassed failed SOCKS proxy")
	}
}

// fakeDownloadProxy handles SOCKS locally; no target connection is ever made.
func fakeDownloadProxy(t *testing.T, greetingDelay time.Duration, response string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(2 * time.Second))
				if _, err := io.ReadFull(c, make([]byte, 3)); err != nil {
					return
				}
				time.Sleep(greetingDelay)
				if _, err := c.Write([]byte{5, 0}); err != nil {
					return
				}
				head := make([]byte, 5)
				if _, err := io.ReadFull(c, head); err != nil {
					return
				}
				if _, err := io.CopyN(io.Discard, c, int64(head[4])+2); err != nil {
					return
				}
				if _, err := c.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 80}); err != nil {
					return
				}
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if line == "\r\n" {
						break
					}
				}
				c.Write([]byte(response))
				// Stall until the client closes, including after sending partial body data.
				io.Copy(io.Discard, c)
			}()
		}
	}()
	return ln.Addr().String()
}

func TestDownloadForHandshakeUsesAbsoluteBudget(t *testing.T) {
	addr := fakeDownloadProxy(t, 120*time.Millisecond, "HTTP/1.1 200 OK\r\n\r\nhello")
	start := time.Now()
	r, err := DownloadFor(addr, "http://example.invalid/", 200*time.Millisecond, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if elapsed > 290*time.Millisecond {
		t.Fatalf("handshake extended budget: %s", elapsed)
	}
	if r.Bytes != 5 || r.Seconds < .17 {
		t.Fatalf("inconsistent measurement: %+v", r)
	}
}

func TestDownloadForTimeoutBoundsBody(t *testing.T) {
	addr := fakeDownloadProxy(t, 0, "HTTP/1.1 200 OK\r\n\r\nhello")
	start := time.Now()
	r, err := DownloadFor(addr, "http://example.invalid/", time.Second, 100*time.Millisecond)
	if err != nil || r.Bytes != 5 {
		t.Fatalf("result=%+v error=%v", r, err)
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("timeout ignored: %s", elapsed)
	}
}

func TestDownloadForCancellation(t *testing.T) {
	for _, delay := range []time.Duration{0, time.Second} {
		t.Run(delay.String(), func(t *testing.T) {
			addr := fakeDownloadProxy(t, delay, "HTTP/1.1 200 OK\r\n\r\nhello")
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(60*time.Millisecond, cancel)
			defer cancel()
			start := time.Now()
			_, err := DownloadForContext(ctx, addr, "http://example.invalid/", time.Second, time.Second)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("expected cancellation, got %v", err)
			}
			if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
				t.Fatalf("cancellation delayed: %s", elapsed)
			}
		})
	}
}

func TestDownloadByteLimit(t *testing.T) {
	addr := fakeDownloadProxy(t, 0, "HTTP/1.1 200 OK\r\n\r\n"+strings.Repeat("x", 10000))
	r, err := Download(addr, "http://example.invalid/", 17, time.Second)
	if err != nil || r.Bytes != 17 {
		t.Fatalf("result=%+v error=%v", r, err)
	}
}

func TestDownloadForTLSHandshakeBudget(t *testing.T) {
	addr := fakeDownloadProxy(t, 0, "")
	start := time.Now()
	_, err := DownloadFor(addr, "https://example.invalid/", 100*time.Millisecond, time.Second)
	if err == nil {
		t.Fatal("expected stalled TLS handshake to fail")
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("TLS handshake exceeded budget: %s", elapsed)
	}
}
