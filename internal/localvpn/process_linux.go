package localvpn

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type ProcessResult struct {
	Code   *int
	Reason string
}

// RunProcess isolates a command in its own session and drains the whole group
// before returning, including children surviving an exited leader. No shell
// command construction is involved. Writers must be bounded by their caller.
func RunProcess(ctx context.Context, executable string, args, environment []string, stdout, stderr io.Writer, grace time.Duration) ProcessResult {
	return runProcess(ctx, executable, args, environment, stdout, stderr, grace, true)
}

func runProcess(ctx context.Context, executable string, args, environment []string, stdout, stderr io.Writer, grace time.Duration, newSession bool) ProcessResult {
	if ctx.Err() != nil {
		return ProcessResult{Reason: contextReason(ctx)}
	}
	// Adopt orphaned grandchildren so termination does not depend on the host's
	// init promptly reaping zombies. Wait4 below targets only this operation PGID.
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return ProcessResult{Reason: "unavailable"}
	}
	cmd := exec.Command(executable, args...)
	cmd.Env = environment
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: newSession, Setpgid: !newSession}
	cmd.WaitDelay = 250 * time.Millisecond
	if err := cmd.Start(); err != nil {
		return ProcessResult{Reason: "unavailable"}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	pgid := cmd.Process.Pid
	var err error
	select {
	case err = <-done:
		// Even a successful leader may leave descendants behind. They must not
		// mutate the system after the UI unlocks another operation.
		drainGroup(pgid, grace)
	case <-ctx.Done():
		_ = unix.Kill(-pgid, unix.SIGTERM)
		timer := time.NewTimer(grace)
		select {
		case <-done:
		case <-timer.C:
			_ = unix.Kill(-pgid, unix.SIGKILL)
			<-done
		}
		timer.Stop()
		drainGroup(pgid, grace)
		return ProcessResult{Reason: contextReason(ctx)}
	}
	code := cmd.ProcessState.ExitCode()
	if err != nil && code == 0 {
		return ProcessResult{Code: &code, Reason: "unavailable"}
	}
	reason := "ok"
	if code != 0 {
		reason = "failed"
	}
	return ProcessResult{Code: &code, Reason: reason}
}

func contextReason(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "timeout"
	}
	return "cancelled"
}

func drainGroup(pgid int, grace time.Duration) {
	if pgid <= 1 {
		return
	}
	reap := func() {
		for {
			var status unix.WaitStatus
			pid, err := unix.Wait4(-pgid, &status, unix.WNOHANG, nil)
			if pid <= 0 || err != nil {
				return
			}
		}
	}
	alive := func() bool { reap(); return !errors.Is(unix.Kill(-pgid, 0), unix.ESRCH) }
	if !alive() {
		return
	}
	_ = unix.Kill(-pgid, unix.SIGTERM)
	deadline := time.Now().Add(grace)
	for alive() {
		if !time.Now().Before(deadline) {
			_ = unix.Kill(-pgid, unix.SIGKILL)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Do not unlock the next operation while an OS-level kill is still pending.
	for alive() {
		time.Sleep(10 * time.Millisecond)
	}
}

type boundedOutput struct {
	mu       sync.Mutex
	bytes    []byte
	limit    int
	overflow bool
	cancel   context.CancelFunc
}

func (w *boundedOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	remaining := w.limit - len(w.bytes)
	if len(p) > remaining {
		p = p[:remaining]
		w.overflow = true
		if w.cancel != nil {
			w.cancel()
		}
	}
	w.bytes = append(w.bytes, p...)
	return n, nil
}
func (w *boundedOutput) result() ([]byte, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.bytes...), w.overflow
}
