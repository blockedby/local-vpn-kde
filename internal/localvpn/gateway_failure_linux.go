package localvpn

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// gatewayStartupError carries only fixed, non-secret labels across the CLI
// boundary. The wrapped cause is retained for cancellation checks, never logged.
type gatewayStartupError struct {
	stage, reason string
	cause         error
}

func (e *gatewayStartupError) Error() string { return "native gateway startup failed" }
func (e *gatewayStartupError) Unwrap() error { return e.cause }

func gatewayFailure(stage, reason string, cause error) error {
	return &gatewayStartupError{stage: stage, reason: reason, cause: cause}
}

func gatewayReadinessFailure(stage string, err error) error {
	reason := "failed"
	switch {
	case errors.Is(err, errRuntimeChildExited):
		reason = "exited"
	case errors.Is(err, errRuntimeReadinessTimeout), errors.Is(err, context.DeadlineExceeded):
		reason = "timeout"
	case errors.Is(err, context.Canceled):
		reason = "cancelled"
	}
	return gatewayFailure(stage, reason, err)
}

// NativeGatewayFailureMessage never includes an underlying error or process
// output, which may contain server endpoints, links, or credentials.
func NativeGatewayFailureMessage(err error) string {
	stage, reason := "unknown", "failed"
	var failure *gatewayStartupError
	if errors.As(err, &failure) {
		switch failure.stage {
		case "config-sync", "config-check", "sing-box", "barrier", "openvpn", "routing", "health", "notify":
			stage = failure.stage
		}
		switch failure.reason {
		case "failed", "exited", "timeout", "cancelled", "unavailable", "rule-init-dns", "rule-init-timeout", "rule-init-refused", "rule-init-network", "rule-init-failed":
			reason = failure.reason
		}
	}
	return fmt.Sprintf("native_gateway_start_failure=%s-%s: gateway startup failed during %s (%s)", stage, reason, stage, reason)
}

// Inspect complete child log lines without retaining or forwarding them. Only
// fixed categories from an explicitly reported rule-set initialization failure are
// exposed; the line itself can contain secrets. A category is evidence of what
// the child reported, not proof that an upstream service was at fault.
type gatewayChildDiagnostics struct {
	mu             sync.Mutex
	line           string
	classification string
}

func (d *gatewayChildDiagnostics) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, b := range p {
		if b == '\n' {
			d.inspect(d.line)
			d.line = ""
		} else if len(d.line) < 8192 {
			d.line += string(b)
		}
	}
	return len(p), nil
}

func (d *gatewayChildDiagnostics) inspect(line string) {
	line = strings.ToLower(line)
	if !(strings.Contains(line, "rule-set") || strings.Contains(line, "rule set")) || !(strings.Contains(line, "download") || strings.Contains(line, "fetch") || strings.Contains(line, "initialize")) {
		return
	}
	if !(strings.Contains(line, "fatal") || strings.Contains(line, "error") || strings.Contains(line, "failed")) {
		return
	}
	switch {
	case strings.Contains(line, "no such host"), strings.Contains(line, "name resolution"), strings.Contains(line, "lookup "):
		d.classification = "rule-init-dns"
	case strings.Contains(line, "timeout"), strings.Contains(line, "timed out"), strings.Contains(line, "deadline exceeded"):
		d.classification = "rule-init-timeout"
	case strings.Contains(line, "connection refused"):
		d.classification = "rule-init-refused"
	case strings.Contains(line, "network is unreachable"), strings.Contains(line, "no route to host"):
		d.classification = "rule-init-network"
	default:
		d.classification = "rule-init-failed"
	}
}

func (d *gatewayChildDiagnostics) reason() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.line != "" {
		d.inspect(d.line)
	}
	if d.classification == "" {
		return "exited"
	}
	return d.classification
}
