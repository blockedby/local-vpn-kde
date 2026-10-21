//go:build linux

package localvpn

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"time"
)

func nativeReady(ctx context.Context) error {
	return notifyNativeReady(ctx, os.Getenv("NOTIFY_SOCKET"))
}

func notifyNativeReady(ctx context.Context, socket string) error {
	// Direct disposable-lab execution is not managed by systemd.
	if socket == "" {
		return nil
	}
	if len(socket) > 107 || strings.ContainsRune(socket, 0) || (socket[0] != '/' && socket[0] != '@') {
		return errors.New("invalid service notification socket")
	}
	if socket[0] == '@' {
		socket = "\x00" + socket[1:]
	}
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(bounded, "unixgram", socket)
	if err != nil {
		return errors.New("service readiness notification failed")
	}
	defer conn.Close()
	deadline, _ := bounded.Deadline()
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	_, err = conn.Write([]byte("READY=1"))
	if err != nil {
		return errors.New("service readiness notification failed")
	}
	return nil
}
