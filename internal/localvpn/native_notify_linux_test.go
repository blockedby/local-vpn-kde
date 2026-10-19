//go:build linux

package localvpn

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestNativeReadyNotification(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "notify.sock")
	listener, err := net.ListenPacket("unixgram", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := notifyNativeReady(context.Background(), socket); err != nil {
		t.Fatal(err)
	}
	if err := listener.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 32)
	n, _, err := listener.ReadFrom(data)
	if err != nil || string(data[:n]) != "READY=1" {
		t.Fatalf("readiness=%q err=%v", data[:n], err)
	}
}

func TestNativeReadyRejectsInvalidSocket(t *testing.T) {
	for _, socket := range []string{"relative.sock", "/tmp/a\x00b"} {
		if err := notifyNativeReady(context.Background(), socket); err == nil {
			t.Fatal("invalid socket accepted")
		}
	}
	if err := notifyNativeReady(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
}
