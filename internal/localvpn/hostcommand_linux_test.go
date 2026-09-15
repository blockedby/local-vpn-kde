package localvpn

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestHostCommandChild(t *testing.T) {
	mode := os.Getenv("LOCAL_VPN_HOST_COMMAND_CHILD")
	if mode == "" {
		return
	}
	fmt.Printf("%d %d", os.Getpid(), unix.Getpgrp())
	if mode == "wait" {
		time.Sleep(time.Minute)
	}
	os.Exit(0)
}

func TestHostCommandPreservesSupervisionAndCancels(t *testing.T) {
	for _, mode := range []string{"exit", "wait"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("LOCAL_VPN_HOST_COMMAND_CHILD", mode)
			timeout := 500 * time.Millisecond
			if mode == "exit" {
				// The race detector delays a clean subprocess exit by one second.
				timeout = 3 * time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			data, err := hostCommand(ctx, os.Args[0], "-test.run=^TestHostCommandChild$")
			if (err != nil) != (mode == "wait") {
				t.Fatalf("unexpected command result: %v", err)
			}
			fields := strings.Fields(string(data))
			if len(fields) != 2 {
				t.Fatalf("child did not report its identity: %q", data)
			}
			pid, _ := strconv.Atoi(fields[0])
			group, _ := strconv.Atoi(fields[1])
			if group != unix.Getpgrp() {
				t.Fatal("host utility escaped the lifecycle process group")
			}
			if pid <= 1 || unix.Kill(pid, 0) != unix.ESRCH {
				t.Fatal("host utility survived command completion")
			}
		})
	}
}
