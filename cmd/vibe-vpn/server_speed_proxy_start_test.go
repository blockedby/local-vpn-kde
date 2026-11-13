//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/blockedby/local-vpn-kde/internal/config"
	"github.com/blockedby/local-vpn-kde/internal/vless"
)

// TestSlowBenchmarkBackendHelper runs only in the subprocess started below.
// It models a backend that creates its real listening socket after a slow
// start; the parent still exercises the production process and readiness path.
func TestSlowBenchmarkBackendHelper(t *testing.T) {
	if os.Getenv("VPNKIT_TEST_SLOW_BACKEND") != "1" {
		return
	}
	data, err := os.ReadFile(os.Args[len(os.Args)-1])
	if err != nil {
		os.Exit(2)
	}
	var cfg struct {
		Inbounds []struct {
			Listen string `json:"listen"`
			Port   int    `json:"listen_port"`
		} `json:"inbounds"`
	}
	if json.Unmarshal(data, &cfg) != nil || len(cfg.Inbounds) != 1 {
		os.Exit(2)
	}
	time.Sleep(3500 * time.Millisecond)
	listener, err := net.Listen("tcp", net.JoinHostPort(cfg.Inbounds[0].Listen, strconv.Itoa(cfg.Inbounds[0].Port)))
	if err != nil {
		os.Exit(2)
	}
	defer listener.Close()
	for {
		conn, err := listener.Accept()
		if err != nil {
			os.Exit(2)
		}
		conn.Close()
	}
}

func TestSpeedProxyWaitsForSlowBackendAndHonorsCancellation(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("VPNKIT_TEST_BACKEND_BINARY", binary)
	script := filepath.Join(t.TempDir(), "slow-backend")
	const helper = "#!/bin/sh\nVPNKIT_TEST_SLOW_BACKEND=1 exec \"$VPNKIT_TEST_BACKEND_BINARY\" -test.run=^TestSlowBenchmarkBackendHelper$ -- \"$3\"\n"
	if err := os.WriteFile(script, []byte(helper), 0700); err != nil {
		t.Fatal(err)
	}
	newConfig := func(t *testing.T) config.Config {
		t.Helper()
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address := listener.Addr().String()
		listener.Close()
		return config.Config{SingBoxBin: script, TestSocks: address}
	}
	node := vless.Node{Outbound: map[string]any{"type": "direct"}}
	t.Run("slow listener becomes ready", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		called := false
		err := withSpeedProxy(ctx, newConfig(t), node, func() error { called = true; return nil })
		if err != nil || !called {
			t.Fatalf("slow backend never reached its check: called=%t err=%v", called, err)
		}
	})
	t.Run("cancel while waiting", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		called := false
		err := withSpeedProxy(ctx, newConfig(t), node, func() error { called = true; return nil })
		if !errors.Is(err, context.DeadlineExceeded) || called {
			t.Fatalf("cancelled backend reached check or returned wrong error: called=%t err=%v", called, err)
		}
	})
}
