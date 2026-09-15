package localvpn

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

var diagnosticReasons = []struct{ message, reason string }{
	{"A vpnkit-local profile already exists", "foreign-profile"},
	{"Disconnect the previous local VPN before migrating", "previous-profile-active"},
	{"underlay installation failed", "underlay-install-failed"},
	{"underlay verification failed", "underlay-verify-failed"},
	{"NetworkManager profile import failed", "profile-import-failed"},
	{"private local assets could not be prepared", "assets-failed"},
	{"local container start failed", "gateway-start-failed"},
	{"lifecycle recovery preflight failed", "recovery_required"}, {"lifecycle committed post-state is unverifiable", "recovery_required"}, {"lifecycle recovery required before backend-only start", "recovery_required"},
	{"failed to resolve source metadata", "docker-registry-failed"}, {"failed to solve:", "gateway-build-failed"},
	{"IPv4 route did not use the exact local VPN device", "route-conflict"}, {"IPv4 route lookup returned malformed output", "route-invalid"},
	{"DNS hostname smoke failed", "dns-failed"}, {"DNS hostname smoke returned no addresses", "dns-failed"},
	{"literal-IP HTTPS smoke failed", "ip-https-failed"}, {"hostname HTTPS smoke failed", "hostname-https-failed"}, {"IPv4 ping smoke failed", "ping-failed"},
	{"IPv6 route is unexpectedly available", "ipv6-leak"}, {"IPv6 ping unexpectedly succeeded", "ipv6-leak"}, {"same-host local VPN smoke failed", "host-smoke-failed"},
	{"NetworkManager activation failed", "nm-activation-failed"}, {"local vpnkit stack failed to start", "gateway-start-failed"}, {"local vpnkit failed closed before readiness", "gateway-unhealthy"},
	{"install and verify the vpnkit local underlay helper", "underlay-not-ready"},
}
var phasePattern = regexp.MustCompile(`vpnkit_phase=([a-z-]+)`)
var phases = map[string]bool{"preparing": true, "prepared": true, "render": true, "compose-up": true, "compose-up-done": true, "nm-work": true, "host-smoke": true, "nm-disconnect": true, "compose-down": true, "runtime-wait": true, "committing": true, "committed": true, "compensating": true, "setup-assets": true, "setup-underlay": true, "setup-gateway": true, "setup-profile": true, "setup-verify": true}

const attemptLimit = 8 * 1024 * 1024

type attemptOutput struct {
	mu                       sync.Mutex
	file                     *os.File
	head                     int
	tail                     []byte
	carry, reason, lastPhase string
	truncated, captureError  bool
	progress                 func(string)
}

func (w *attemptOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	text := w.carry + string(p)
	if w.reason == "" {
		for _, entry := range diagnosticReasons {
			if strings.Contains(text, entry.message) {
				w.reason = entry.reason
				break
			}
		}
	}
	if w.progress != nil {
		for _, match := range phasePattern.FindAllStringSubmatch(text, -1) {
			phase := match[1]
			if phases[phase] && phase != w.lastPhase {
				w.progress(phase)
				w.lastPhase = phase
			}
		}
	}
	if len(text) > 256 {
		text = text[len(text)-256:]
	}
	w.carry = text
	room := attemptLimit/2 - w.head
	if room > len(p) {
		room = len(p)
	}
	if room > 0 {
		if _, err := w.file.Write(p[:room]); err != nil {
			w.captureError = true
		}
		w.head += room
	}
	if room < len(p) {
		w.truncated = true
		w.tail = append(w.tail, p[room:]...)
		if len(w.tail) > attemptLimit/2 {
			w.tail = append([]byte(nil), w.tail[len(w.tail)-attemptLimit/2:]...)
		}
	}
	return n, nil
}
func (b *Bridge) lifecycle(ctx context.Context, action string, args []string, progress func(string)) (ProcessResult, string) {
	failure := ProcessResult{Reason: "diagnostics-unavailable"}
	base, err := directory(b.options.Base, false, true)
	if err != nil {
		return failure, ""
	}
	defer unix.Close(base)
	if privateDirectory(base) != nil {
		return failure, ""
	}
	dir, err := childDirectory(base, "diagnostics", true, true)
	if err != nil {
		return failure, ""
	}
	defer unix.Close(dir)
	if privateDirectory(dir) != nil {
		return failure, ""
	}
	var random [6]byte
	if _, err = rand.Read(random[:]); err != nil {
		return failure, ""
	}
	started := time.Now()
	id := started.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(random[:])
	fd, err := unix.Openat(dir, id+".log", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return failure, ""
	}
	file := os.NewFile(uintptr(fd), id+".log")
	defer file.Close()
	if err = validatePrivateFile(fd); err != nil {
		return failure, ""
	}
	output := &attemptOutput{file: file, progress: progress}
	metadata := func(state string, result ProcessResult) error {
		payload, err := json.Marshal(map[string]any{"schema": 1, "attempt_id": id, "action": action, "started_unix": float64(started.UnixNano()) / 1e9, "elapsed_seconds": time.Since(started).Seconds(), "state": state, "returncode": result.Code, "reason": result.Reason, "truncated": output.truncated, "capture_error": output.captureError})
		if err != nil {
			return err
		}
		return atomicWrite(dir, id+".json", append(payload, '\n'))
	}
	if err = metadata("running", ProcessResult{}); err != nil {
		return failure, ""
	}
	result := runProcess(ctx, b.options.Executable, args, append(os.Environ(), "VPNKIT_TUI_SUPERVISED=1", "VPNKIT_TUI_DIAGNOSTICS=1"), output, output, b.options.Grace, !b.options.keepTerminal)
	if output.truncated {
		if _, err = file.Write(output.tail); err != nil {
			output.captureError = true
		}
	}
	if err = file.Sync(); err != nil {
		output.captureError = true
	}
	if result.Reason == "failed" && output.reason != "" {
		result.Reason = output.reason
	}
	if err = metadata("finished", result); err != nil {
		if result.Reason == "ok" {
			result.Reason = "diagnostics-unavailable"
		}
	}
	return result, id
}
