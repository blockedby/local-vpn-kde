package nettest

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func speedSOCKSFixture(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer conn.Close()
				stop := context.AfterFunc(ctx, func() { conn.Close() })
				defer stop()
				greeting := make([]byte, 3)
				if _, err := io.ReadFull(conn, greeting); err != nil {
					return
				}
				conn.Write([]byte{5, 0})
				request := make([]byte, 5)
				if _, err := io.ReadFull(conn, request); err != nil {
					return
				}
				if _, err := io.CopyN(io.Discard, conn, int64(request[4])+2); err != nil {
					return
				}
				conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 80})
				reader := bufio.NewReader(conn)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					if line == "\r\n" {
						break
					}
				}
				if _, err := fmt.Fprint(conn, "HTTP/1.1 200 OK\r\nContent-Length: 99999999\r\n\r\n"); err != nil {
					return
				}
				for {
					select {
					case <-ctx.Done():
						return
					case <-time.After(20 * time.Millisecond):
						if _, err := io.WriteString(conn, strings.Repeat("a", 1000)); err != nil {
							return
						}
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { cancel(); listener.Close(); workers.Wait() })
	return listener.Addr().String()
}

func TestDownloadSpeedProgressIsMeasuredBeforeCompletion(t *testing.T) {
	address := speedSOCKSFixture(t)
	var samples []Result
	result, err := DownloadForContext(context.Background(), address, "http://example.test/file", 420*time.Millisecond, time.Second, func(r Result) error { samples = append(samples, r); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) < 3 || len(samples) > 7 {
		t.Fatalf("unbounded or missing progress: %d", len(samples))
	}
	if samples[0].Seconds >= result.Seconds || samples[0].Bytes >= result.Bytes {
		t.Fatal("progress only arrived at completion")
	}
	var previous Result
	for _, r := range samples {
		if r.Bytes < previous.Bytes || r.Seconds <= previous.Seconds || r.Bytes <= 0 || math.Abs(r.Mbps-float64(r.Bytes)*8/r.Seconds/1e6) > 1e-9 {
			t.Fatal("not actual cumulative body evidence", r)
		}
		previous = r
	}
	if previous.Bytes != result.Bytes || math.Abs(result.Mbps-float64(result.Bytes)*8/result.Seconds/1e6) > 1e-9 {
		t.Fatal("final mean changed", result)
	}
}

func TestDownloadSpeedProgressCancellationAndWriterFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			address := speedSOCKSFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			marker := errors.New("progress writer closed")
			calls := 0
			started := time.Now()
			_, err := DownloadForContext(ctx, address, "http://example.test/file", 3*time.Second, time.Second, func(r Result) error {
				calls++
				if fail {
					return marker
				}
				cancel()
				return nil
			})
			if fail && !errors.Is(err, marker) {
				t.Fatal(err)
			}
			if !fail && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if calls != 1 || time.Since(started) > time.Second {
				t.Fatal("cancellation or callback failure kept download running", calls)
			}
		})
	}
}
