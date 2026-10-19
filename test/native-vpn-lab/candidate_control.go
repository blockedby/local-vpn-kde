//go:build linux

package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/blockedby/local-vpn-kde/internal/nativegateway"
	"github.com/blockedby/local-vpn-kde/internal/singbox"
	"github.com/blockedby/local-vpn-kde/internal/vless"
)

const realControlSOCKSPort = 21801

func candidateControlObservation(ordinal int, classification string) string {
	fixed := map[string]bool{
		"success": true, "unsupported": true, "endpoint-resolution": true,
		"config-build": true, "config-write": true, "process-start": true,
		"process-exit": true, "readiness-timeout": true, "empty-body": true,
		"request-build": true, "tls-validation": true, "dial-refused": true,
		"connection-reset": true, "dial-unreachable": true, "tls-eof": true,
		"dial-timeout": true, "tls-protocol": true, "transport": true,
		"http-status": true, "body-read": true,
	}
	if !fixed[classification] {
		classification = "transport"
	}
	return "lab_observation=real-underlay-control candidate=" + strconv.Itoa(ordinal) + " classification=" + classification
}

// buildCandidateControlConfig creates the smallest useful A-side configuration:
// the candidate is unchanged except for its local tag, there is no TUN, and all
// traffic accepted by the loopback SOCKS listener uses that exact candidate.
func buildCandidateControlConfig(prepared map[string]any) ([]byte, error) {
	if prepared == nil {
		return nil, errors.New("missing prepared candidate")
	}
	outbound := make(map[string]any, len(prepared)+1)
	for key, value := range prepared {
		outbound[key] = value
	}
	outbound["tag"] = "selected-native-out"
	config := map[string]any{
		"log": map[string]any{"disabled": true},
		"inbounds": []any{map[string]any{
			"type": "socks", "tag": "candidate-control-socks",
			"listen": "127.0.0.1", "listen_port": realControlSOCKSPort,
		}},
		"outbounds": []any{outbound},
		"route":     map[string]any{"final": "selected-native-out"},
	}
	return json.MarshalIndent(config, "", "  ")
}

func prepareRealCandidate(ctx context.Context, candidate realCandidate) (realCandidate, string) {
	outbound, err := vless.SingBoxOutbound(candidate.link)
	if err != nil {
		return candidate, "unsupported"
	}
	// Resolve in the parent container exactly once. The resulting IP endpoint and
	// untouched TLS/Reality/transport metadata are shared by both A and B.
	outbound, err = singbox.PrepareOutboundForNamespace(ctx, outbound)
	if err != nil {
		return candidate, "endpoint-resolution"
	}
	candidate.outbound = outbound
	return candidate, "success"
}

func runCandidateUnderlayControl(ctx context.Context, candidate realCandidate, probeIP string) string {
	return runCandidateUnderlayControlTarget(ctx, candidate, benchmarkTarget{URL: "https://" + realProbeHost + realProbePath, Hostname: realProbeHost, IP: probeIP})
}

func runCandidateUnderlayControlTarget(ctx context.Context, candidate realCandidate, target benchmarkTarget) string {
	encoded, err := buildCandidateControlConfig(candidate.outbound)
	if err != nil {
		return "config-build"
	}
	dir, err := os.MkdirTemp("/run", "native-vpn-control-")
	if err != nil {
		return "config-write"
	}
	defer os.RemoveAll(dir)
	if os.Chmod(dir, 0700) != nil || os.Chown(dir, serviceUID, serviceGID) != nil {
		return "config-write"
	}
	path := filepath.Join(dir, "config.json")
	if os.WriteFile(path, append(encoded, '\n'), 0600) != nil || os.Chown(path, serviceUID, serviceGID) != nil || os.Chmod(path, 0600) != nil {
		return "config-write"
	}

	control, classification := startCandidateControl(ctx, path)
	if classification != "success" {
		return classification
	}
	defer control.stopWithGrace(3 * time.Second)
	if classification = waitCandidateControlReady(ctx, control); classification != "success" {
		return classification
	}

	probe, requestErr := realHTTPSGetViaSOCKSTarget(ctx, target, 12*time.Second)
	if requestErr != nil {
		return probe.classification
	}
	if probe.bodyBytes == 0 {
		return "empty-body"
	}
	return "success"
}

type candidateControlProcess struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	done   chan error
}

func startCandidateControl(parent context.Context, configPath string) (*candidateControlProcess, string) {
	ctx, cancel := context.WithCancel(parent)
	args := []string{
		"--reuid", strconv.Itoa(serviceUID), "--regid", strconv.Itoa(serviceGID), "--clear-groups",
		"--bounding-set", "-all", "--no-new-privs",
		nativegateway.LibexecDir + "/sing-box", "run", "-c", configPath,
	}
	cmd := exec.CommandContext(ctx, "setpriv", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, "process-start"
	}
	control := &candidateControlProcess{cmd: cmd, cancel: cancel, done: make(chan error, 1)}
	go func() {
		control.done <- cmd.Wait()
		close(control.done)
	}()
	return control, "success"
}

func waitCandidateControlReady(parent context.Context, control *candidateControlProcess) string {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		connection, err := (&net.Dialer{Timeout: 200 * time.Millisecond}).DialContext(ctx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(realControlSOCKSPort)))
		if err == nil {
			_ = connection.Close()
			return "success"
		}
		select {
		case <-control.done:
			return "process-exit"
		case <-ctx.Done():
			return "readiness-timeout"
		case <-ticker.C:
		}
	}
}

func (control *candidateControlProcess) stopWithGrace(grace time.Duration) {
	if control == nil {
		return
	}
	if control.cmd.Process != nil {
		_ = syscall.Kill(-control.cmd.Process.Pid, syscall.SIGTERM)
	}
	select {
	case <-control.done:
	case <-time.After(grace):
		control.cancel()
		if control.cmd.Process != nil {
			_ = syscall.Kill(-control.cmd.Process.Pid, syscall.SIGKILL)
		}
		select {
		case <-control.done:
		case <-time.After(time.Second):
		}
	}
	control.cancel()
}

func realHTTPSGetViaSOCKS(parent context.Context, targetIP string, timeout time.Duration) (realHTTPSResult, error) {
	return realHTTPSGetViaSOCKSTarget(parent, benchmarkTarget{URL: "https://" + realProbeHost + realProbePath, Hostname: realProbeHost, IP: targetIP}, timeout)
}

func realHTTPSGetViaSOCKSTarget(parent context.Context, target benchmarkTarget, timeout time.Duration) (realHTTPSResult, error) {
	return realHTTPSGetTargetWithDial(parent, target, timeout, func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialSOCKS5(ctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(realControlSOCKSPort)), target.IP, 443)
	})
}

func dialSOCKS5(ctx context.Context, proxyAddress, targetIP string, targetPort uint16) (net.Conn, error) {
	ip := net.ParseIP(targetIP).To4()
	if ip == nil {
		return nil, errors.New("SOCKS target is not IPv4")
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp4", proxyAddress)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = connection.Close()
		}
	}()
	if deadline, exists := ctx.Deadline(); exists {
		_ = connection.SetDeadline(deadline)
	}
	if _, err = connection.Write([]byte{5, 1, 0}); err != nil {
		return nil, err
	}
	response := make([]byte, 2)
	if _, err = io.ReadFull(connection, response); err != nil || response[0] != 5 || response[1] != 0 {
		return nil, errors.New("SOCKS authentication failed")
	}
	request := []byte{5, 1, 0, 1, ip[0], ip[1], ip[2], ip[3], 0, 0}
	binary.BigEndian.PutUint16(request[8:], targetPort)
	if _, err = connection.Write(request); err != nil {
		return nil, err
	}
	header := make([]byte, 4)
	if _, err = io.ReadFull(connection, header); err != nil || header[0] != 5 || header[1] != 0 {
		return nil, errors.New("SOCKS connect failed")
	}
	addressBytes := 0
	switch header[3] {
	case 1:
		addressBytes = 4
	case 4:
		addressBytes = 16
	case 3:
		length := []byte{0}
		if _, err = io.ReadFull(connection, length); err != nil {
			return nil, err
		}
		addressBytes = int(length[0])
	default:
		return nil, errors.New("SOCKS response address invalid")
	}
	if _, err = io.ReadFull(connection, make([]byte, addressBytes+2)); err != nil {
		return nil, err
	}
	_ = connection.SetDeadline(time.Time{})
	ok = true
	return connection, nil
}
