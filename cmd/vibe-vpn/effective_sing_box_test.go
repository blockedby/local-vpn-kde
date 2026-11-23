package main

import (
	"os"
	"testing"

	"github.com/blockedby/local-vpn-kde/internal/config"
	"github.com/blockedby/local-vpn-kde/internal/vless"
)

func TestCustomSingBoxBinaryForChecksAndSwitches(t *testing.T) {
	const binary = "/usr/local/bin/sing-box"
	c := config.Config{SingBoxBin: binary, TestSocks: "127.0.0.1:1080"}
	backend, err := tempBenchmarkBackend(c, vless.Node{Outbound: map[string]any{"type": "direct"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(backend.configPath) })
	if backend.bin != binary {
		t.Errorf("benchmark binary = %q, want %q", backend.bin, binary)
	}
	if got := singboxRestartConfig(c).SingBoxBin; got != binary {
		t.Errorf("restart binary = %q, want %q", got, binary)
	}
}
