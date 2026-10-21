//go:build linux

package localvpn

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/blockedby/local-vpn-kde/internal/nativegateway"
)

func TestNativeCommandsRejectExecutableInjection(t *testing.T) {
	for _, name := range []string{"sh", "/usr/bin/ip", "ip; true", "docker", "systemctl", "../ip"} {
		if _, err := nativeSystemExecutable(name); err == nil {
			t.Fatalf("accepted executable %q", name)
		}
	}
}

func TestNativeCommandRejectsInputBeforeExecution(t *testing.T) {
	// An unallowlisted program is rejected without looking up or running it.
	if _, err := nativeCommand(context.Background(), nativegateway.Command{Program: "sh", Args: []string{"-c", "exit 0"}, Stdin: "payload"}); err == nil {
		t.Fatal("accepted arbitrary program")
	}
}

func TestProcessInput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var out bytes.Buffer
	result := runProcessInput(ctx, "/bin/cat", nil, []string{"PATH=/usr/bin:/bin"}, strings.NewReader("synthetic-input\n"), &out, io.Discard, time.Millisecond*100, true)
	if result.Reason != "ok" || out.String() != "synthetic-input\n" {
		t.Fatalf("input not delivered: reason=%s output=%q", result.Reason, out.String())
	}
}

func TestNativeIPTablesAppletName(t *testing.T) {
	if _, err := nativeSystemExecutable("iptables"); err != nil {
		t.Skip("trusted iptables unavailable")
	}
	out, err := nativeCommand(context.Background(), nativegateway.Command{Program: "iptables", Args: []string{"--version"}})
	if err != nil || !strings.Contains(string(out), "iptables v") {
		t.Fatalf("iptables applet failed: %q %v", out, err)
	}
}
