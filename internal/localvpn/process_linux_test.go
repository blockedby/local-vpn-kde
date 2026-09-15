package localvpn

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestRunProcessDrainsSurvivingDescendant(t *testing.T) {
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "descendant")
	// The shell is deliberately adversarial fixture code, never runtime logic.
	script := "trap '' TERM; sleep 60 & echo $! > \"$1\"; exit 0"
	result := RunProcess(context.Background(), "bash", []string{"-c", script, "fixture", pidfile}, os.Environ(), nil, nil, 50*time.Millisecond)
	if result.Reason != "ok" {
		t.Fatalf("result: %+v", result)
	}
	raw, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if err = unix.Kill(pid, 0); err != unix.ESRCH {
		t.Fatalf("descendant survived: %v", err)
	}
}

func TestRunProcessTimeoutDrainsGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	result := RunProcess(ctx, "bash", []string{"-c", "trap '' TERM; sleep 60 & wait"}, os.Environ(), nil, nil, 50*time.Millisecond)
	if result.Reason != "timeout" {
		t.Fatalf("result: %+v", result)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("timeout did not bound termination")
	}
}

func TestRunProcessBoundsOutput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output := &boundedOutput{limit: 128, cancel: cancel}
	result := RunProcess(ctx, "bash", []string{"-c", "while :; do printf 'XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX'; done"}, os.Environ(), output, nil, 50*time.Millisecond)
	data, overflow := output.result()
	if len(data) != 128 || !overflow || result.Reason != "cancelled" {
		t.Fatalf("overflow not stopped: len=%d overflow=%v result=%+v", len(data), overflow, result)
	}
}

func TestRunProcessNeverInterpretsArguments(t *testing.T) {
	cat, err := exec.LookPath("printf")
	if err != nil {
		t.Fatal(err)
	}
	output := &boundedOutput{limit: 128}
	value := "$(touch should-not-exist); secret"
	result := RunProcess(context.Background(), cat, []string{"%s", value}, os.Environ(), output, nil, time.Second)
	raw, _ := output.result()
	if result.Reason != "ok" || string(raw) != value {
		t.Fatal("argument contract changed")
	}
}
