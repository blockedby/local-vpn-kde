package localvpn

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/blockedby/local-vpn-kde/internal/picker"
	"golang.org/x/sys/unix"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
)

type BridgeOptions struct {
	Base, Executable, Mode string
	Mock                   bool
	Timeout, Grace         time.Duration
	keepTerminal           bool
}
type Bridge struct {
	options     BridgeOptions
	status      map[string]any
	mu          sync.Mutex
	active      map[string]context.CancelFunc
	statusMu    sync.RWMutex
	refreshMu   bridgeGate
	mutationMu  bridgeGate
	operationMu bridgeGate
}

// bridgeGate is a cancellable read/write gate. Waiting never owns a worker's
// cancellation path, so a queued request can finish without waiting for a slow
// lifecycle operation to release its resources.
type bridgeGate struct {
	mu      sync.Mutex
	readers int
	writer  bool
	changed chan struct{}
}

func (g *bridgeGate) acquire(ctx context.Context, exclusive bool) (func(), error) {
	for {
		g.mu.Lock()
		if err := ctx.Err(); err != nil {
			g.mu.Unlock()
			return nil, err
		}
		if !g.writer && (!exclusive || g.readers == 0) {
			if exclusive {
				g.writer = true
			} else {
				g.readers++
			}
			g.mu.Unlock()
			return func() {
				g.mu.Lock()
				if exclusive {
					g.writer = false
				} else {
					g.readers--
				}
				if g.changed != nil {
					close(g.changed)
					g.changed = nil
				}
				g.mu.Unlock()
			}, nil
		}
		if g.changed == nil {
			g.changed = make(chan struct{})
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

var actions = map[string][]string{"backend/start": {"backend", "start"}, "disconnect": {"disconnect"}, "start": {"start"}, "stop": {"stop"}, "retest/select": {"retest", "select"}, "toggle-mode": {"toggle", "mode"}, "diagnostics": {"diagnostics"}}
var serverID = regexp.MustCompile(`^srv_[A-Za-z0-9_-]{27}$`)

func NewBridge(o BridgeOptions) (*Bridge, error) {
	if o.Mode == "" {
		o.Mode = "strict"
	}
	if o.Mode != "strict" && o.Mode != "smart" {
		return nil, errors.New("invalid mode")
	}
	if o.Timeout == 0 {
		o.Timeout = 30 * time.Minute
	}
	if o.Grace == 0 {
		o.Grace = 31 * time.Second
	}
	if o.Timeout < 0 || o.Grace < 0 {
		return nil, errors.New("invalid timeout")
	}
	if !o.Mock {
		if !filepath.IsAbs(o.Executable) || strings.ContainsFunc(o.Executable, unicode.IsSpace) {
			return nil, errors.New("invalid lifecycle adapter")
		}
		for _, name := range []string{"docker", "docker-compose", "podman", "podman-compose", "nmcli", "networkmanager", "sudo", "openvpn", "systemctl", "ip", "iptables", "nft"} {
			if strings.ToLower(filepath.Base(o.Executable)) == name {
				return nil, errors.New("direct system executable refused")
			}
		}
		fd, err := subscriptionParent(o.Base)
		if err != nil {
			return nil, errors.New("private subscription directory unavailable")
		}
		unix.Close(fd)
	}
	kind := "configured"
	if o.Mock {
		kind = "mock"
	}
	b := &Bridge{options: o, active: make(map[string]context.CancelFunc), status: map[string]any{"schema": 1, "vpn_state": "unknown", "gateway_state": "unknown", "subscription": "not configured", "endpoint": "redacted", "routing_mode": o.Mode, "selection": "redacted", "diagnostics": "not-run", "last_action": "none", "last_result": "not-run", "last_attempt": "", "networkmanager_configured": "unknown", "networkmanager_active": "unknown", "runner": kind}}
	if !o.Mock && SubscriptionConfigured(o.Base) {
		b.status["subscription"] = "configured"
	}
	return b, nil
}
func (b *Bridge) Cancel() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, cancel := range b.active {
		cancel()
	}
}
func (b *Bridge) QueryStatus(ctx context.Context) map[string]any { b.refresh(ctx); return b.Status() }

func (b *Bridge) Status() map[string]any {
	b.statusMu.RLock()
	defer b.statusMu.RUnlock()
	copy := map[string]any{}
	for k, v := range b.status {
		copy[k] = v
	}
	return copy
}

// Serve multiplexes ID-bearing requests. Legacy requests retain ordered replies.
// A bounded number of workers keeps malformed clients from creating unlimited
// processes. Cancellation is read by the scanner even while workers are busy.
func (b *Bridge) Serve(ctx context.Context, input io.Reader, output io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), maxSubscription*6+1024)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if index := bytes.IndexByte(data, '\n'); index >= 0 {
			return index + 1, data[:index], nil
		}
		if atEOF && len(data) > 0 {
			return 0, nil, errors.New("incomplete request")
		}
		return 0, nil, nil
	})
	encoder := json.NewEncoder(output)
	var outputMu sync.Mutex
	var firstErr error
	emit := func(id string, value any) error {
		outputMu.Lock()
		defer outputMu.Unlock()
		if firstErr != nil {
			return firstErr
		}
		if id != "" {
			data, err := json.Marshal(value)
			if err != nil {
				firstErr = err
				cancel()
				return err
			}
			var envelope map[string]any
			if err = json.Unmarshal(data, &envelope); err != nil {
				firstErr = err
				cancel()
				return err
			}
			envelope["id"] = id
			value = envelope
		}
		if err := encoder.Encode(value); err != nil {
			firstErr = err
			cancel()
			return err
		}
		return nil
	}
	var workers sync.WaitGroup
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		var head struct {
			ID     string `json:"id"`
			Action string `json:"action"`
		}
		if err := json.Unmarshal(line, &head); err != nil || len(head.ID) > 128 || strings.ContainsFunc(head.ID, unicode.IsControl) {
			_ = emit("", map[string]any{"ok": false, "reason": "invalid-request", "status": b.Status()})
			continue
		}
		if head.Action == "cancel" {
			b.mu.Lock()
			if stop := b.active[head.ID]; stop != nil {
				stop()
			}
			b.mu.Unlock()
			continue
		}
		if head.ID == "" {
			workers.Wait()
		}
		requestCtx, stop := context.WithTimeout(ctx, b.options.Timeout)
		b.mu.Lock()
		_, duplicate := b.active[head.ID]
		full := len(b.active) >= 16
		if !duplicate && !full {
			b.active[head.ID] = stop
		}
		b.mu.Unlock()
		if duplicate {
			stop()
			cancel()
			workers.Wait()
			return errors.New("duplicate request id")
		}
		if full {
			stop()
			_ = emit(head.ID, map[string]any{"ok": false, "reason": "busy", "status": b.Status()})
			continue
		}
		run := func() {
			defer workers.Done()
			defer func() { stop(); b.mu.Lock(); delete(b.active, head.ID); b.mu.Unlock() }()
			// Selection changes only the active proxy. Probes own independent proxies;
			// lifecycle and catalog mutations must wait for those proxies to finish.
			shared := head.Action == "servers/select" || head.Action == "servers/list" || head.Action == "servers/current" || head.Action == "servers/ping" || head.Action == "servers/speed" || head.Action == "servers/availability" || head.Action == "servers/check-batch" || head.Action == "status" || head.Action == "subscription/read"
			mutation := !shared || head.Action == "servers/select"
			canceled := func() {
				reason := "cancelled"
				if errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
					reason = "timeout"
				}
				_ = emit(head.ID, map[string]any{"ok": false, "reason": reason, "status": b.Status()})
			}
			if mutation {
				release, err := b.mutationMu.acquire(requestCtx, true)
				if err != nil {
					canceled()
					return
				}
				defer release()
			}
			release, err := b.operationMu.acquire(requestCtx, !shared)
			if err != nil {
				canceled()
				return
			}
			defer release()
			if requestCtx.Err() != nil {
				canceled()
				return
			}

			reply, err := b.request(requestCtx, line, func(phase string) { _ = emit(head.ID, map[string]string{"event": "progress", "phase": phase}) }, func(p picker.CheckProgress) error { return emit(head.ID, p) })
			if err != nil {
				outputMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				outputMu.Unlock()
				cancel()
				return
			}
			_ = emit(head.ID, reply)
		}
		workers.Add(1)
		if head.ID == "" {
			run()
		} else {
			go run()
		}
	}
	if scanner.Err() != nil {
		cancel()
	}
	workers.Wait()
	if scanner.Err() != nil {
		return scanner.Err()
	}
	outputMu.Lock()
	defer outputMu.Unlock()
	return firstErr
}
func (b *Bridge) request(parent context.Context, line []byte, progress func(string), checkProgress func(picker.CheckProgress) error) (map[string]any, error) {
	ctx := parent
	reply := map[string]any{"ok": true, "reason": "ok", "code": nil}
	invalid := func() { reply["ok"] = false; reply["reason"] = "invalid-request" }
	var request struct {
		Action   string          `json:"action"`
		Value    json.RawMessage `json:"value"`
		Progress bool            `json:"progress"`
	}
	if err := json.Unmarshal(line, &request); err != nil {
		invalid()
		reply["status"] = b.Status()
		return reply, nil
	}
	if !request.Progress {
		progress = nil
		checkProgress = nil
	}
	var value string
	if len(request.Value) > 0 && string(request.Value) != "null" {
		if err := json.Unmarshal(request.Value, &value); err != nil {
			invalid()
			reply["status"] = b.Status()
			return reply, nil
		}
	}
	switch request.Action {
	case "status":
		b.refresh(ctx)
	case "subscription/read":
		text, err := ReadSubscription(b.options.Base)
		if err != nil {
			invalid()
		} else {
			reply["value"] = text
		}
	case "subscription":
		if b.options.Mock {
			invalid()
		} else if err := WriteSubscription(b.options.Base, value); err != nil {
			invalid()
		} else {
			b.statusMu.Lock()
			b.status["subscription"] = "configured"
			b.status["last_action"] = "configure subscription"
			b.status["last_result"] = "ok"
			b.statusMu.Unlock()
		}
	default:
		if strings.HasPrefix(request.Action, "servers/") {
			args, ids, err := serverArguments(request.Action, value)
			if err != nil {
				invalid()
				break
			}
			catalog := map[string]any{"status": "ok", "servers": []any{}}
			if !b.options.Mock {
				var data []byte
				var result ProcessResult
				var overflow bool
				if ids != nil {
					data, result, overflow = b.captureChecks(ctx, args, ids, checkProgress)
				} else {
					data, result, overflow = b.capture(ctx, args, 1048576)
				}
				switch {
				case overflow:
					catalog = map[string]any{"status": "unavailable"}
				case result.Reason == "cancelled":
					catalog = map[string]any{"status": "canceled"}
				case result.Reason == "timeout":
					catalog = map[string]any{"status": "timeout"}
				case result.Reason == "failed":
					// Failed probes still return a bounded, valid catalog response.
					parsed, parseErr := sanitizeCatalog(data, ids)
					catalog = map[string]any{"status": "unavailable"}
					if parseErr == nil {
						switch parsed["status"] {
						case "failed", "stale", "canceled", "recovery_required", "backend-outdated":
							catalog = parsed
						}
					}
				case result.Reason != "ok":
					catalog = map[string]any{"status": "unavailable"}
				default:
					catalog, err = sanitizeCatalog(data, ids)
					if err != nil {
						invalid()
						break
					}
				}
			}
			if err == nil {
				reply["catalog"] = catalog
				reply["ok"] = catalog["status"] == "ok"
				reply["reason"] = catalog["status"]
				if catalog["status"] == "canceled" {
					reply["reason"] = "cancelled"
				}
			}
		} else if args, ok := actions[request.Action]; ok {
			code := 0
			result := ProcessResult{Code: &code, Reason: "ok"}
			attempt := ""
			if !b.options.Mock {
				result, attempt = b.lifecycle(ctx, request.Action, args, progress)
			}
			reply["ok"] = result.Reason == "ok"
			reply["reason"] = result.Reason
			reply["code"] = result.Code
			b.statusMu.Lock()
			b.status["last_action"] = request.Action
			b.status["last_result"] = result.Reason
			b.status["last_attempt"] = attempt
			if result.Reason == "ok" && request.Action == "diagnostics" {
				b.status["diagnostics"] = "available"
			}
			b.statusMu.Unlock()
			b.refresh(ctx)
		} else {
			invalid()
		}
	}
	reply["status"] = b.Status()
	return reply, nil
}
func (b *Bridge) capture(ctx context.Context, args []string, limit int) ([]byte, ProcessResult, bool) {
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	output := &boundedOutput{limit: limit, cancel: cancel}
	result := RunProcess(child, b.options.Executable, args, append(os.Environ(), "VPNKIT_TUI_SUPERVISED=1"), output, nil, b.options.Grace)
	data, overflow := output.result()
	return data, result, overflow
}
func binaryStatus(value any) string {
	if v, ok := value.(bool); ok {
		if v {
			return "yes"
		}
		return "no"
	}
	v, ok := value.(string)
	if !ok {
		return "unknown"
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "yes", "configured", "active", "connected":
		return "yes"
	case "no", "missing", "inactive", "disconnected", "not-configured", "not configured":
		return "no"
	case "not-managed":
		return "not-managed"
	}
	return "unknown"
}
func (b *Bridge) refresh(parent context.Context) {
	release, err := b.refreshMu.acquire(parent, true)
	if err != nil {
		return
	}
	defer release()
	status := map[string]any{}
	defer func() {
		b.statusMu.Lock()
		defer b.statusMu.Unlock()
		for k, v := range status {
			b.status[k] = v
		}
	}()

	status["vpn_state"] = "unknown"
	status["gateway_state"] = "unknown"
	status["networkmanager_configured"] = "unknown"
	status["networkmanager_active"] = "unknown"
	if b.options.Mock {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	data, result, overflow := b.capture(ctx, []string{"status", "--json"}, 16384)
	if overflow || result.Reason != "ok" {
		return
	}
	var raw map[string]any
	if json.Unmarshal(data, &raw) != nil {
		return
	}
	container, _ := raw["container"].(string)
	switch container {
	case "absent", "stopped", "starting", "healthy", "unhealthy", "running", "inactive", "unknown":
		status["gateway_state"] = container
	default:
		container = "unknown"
	}
	if raw["routing_policy"] == "smart" || raw["routing_policy"] == "strict" {
		status["routing_mode"] = raw["routing_policy"]
	}
	if raw["subscription"] == "configured" {
		status["subscription"] = "configured"
	} else if raw["subscription"] == "missing" {
		status["subscription"] = "not configured"
	}
	configured, active := raw["networkmanager_configured"], raw["networkmanager_active"]
	if configured == nil {
		configured = raw["configured"]
	}
	if active == nil {
		active = raw["active"]
	}
	if nm, ok := raw["networkmanager"].(map[string]any); ok {
		if v, present := nm["configured"]; present {
			configured = v
		}
		if v, present := nm["active"]; present {
			active = v
		}
	}
	c, a := binaryStatus(configured), binaryStatus(active)
	status["networkmanager_configured"] = c
	status["networkmanager_active"] = a
	switch {
	case a == "no" && (c == "yes" || c == "no"):
		status["vpn_state"] = "inactive"
	case a == "yes" || c == "not-managed":
		status["vpn_state"] = container
	case container != "healthy" && container != "running":
		status["vpn_state"] = container
	}
}
func validTarget(value string) bool {
	if len(value) > 2048 || strings.ContainsFunc(value, unicode.IsControl) {
		return false
	}
	u, err := url.Parse(value)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil
}
func serverArguments(action, value string) ([]string, []string, error) {
	command := strings.TrimPrefix(action, "servers/")
	args := []string{"servers", command}
	invalid := errors.New("invalid server request")
	switch command {
	case "list", "refresh", "current":
		return args, nil, nil
	case "ping", "speed", "select":
		if !serverID.MatchString(value) {
			return nil, nil, invalid
		}
		return append(args, value), nil, nil
	case "availability":
		var request struct{ ID, URL string }
		if json.Unmarshal([]byte(value), &request) != nil || !serverID.MatchString(request.ID) || !validTarget(request.URL) {
			return nil, nil, invalid
		}
		return append(args, request.ID, request.URL), nil, nil
	case "check-batch":
		var request struct {
			IDs []string
			URL string
		}
		if json.Unmarshal([]byte(value), &request) != nil || len(request.IDs) < 1 || len(request.IDs) > 1000 || !validTarget(request.URL) {
			return nil, nil, invalid
		}
		seen := map[string]bool{}
		for _, id := range request.IDs {
			if !serverID.MatchString(id) || seen[id] {
				return nil, nil, invalid
			}
			seen[id] = true
		}
		return append(args, strings.Join(request.IDs, ","), request.URL), request.IDs, nil
	}
	return nil, nil, invalid
}
func sanitizeCatalog(data []byte, ids []string) (map[string]any, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	if raw["schema"] != "vibe-vpn.server-browser.v2" {
		return map[string]any{"status": "backend-outdated"}, nil
	}
	result := map[string]any{"status": "unavailable"}
	switch raw["status"] {
	case "ok", "failed", "stale", "canceled", "unavailable", "recovery_required", "backend-outdated":
		result["status"] = raw["status"]
	}
	row := func(value any) (map[string]any, error) {
		item, ok := value.(map[string]any)
		if !ok {
			return nil, errors.New("invalid server")
		}
		id, ok := item["server_id"].(string)
		if !ok || !serverID.MatchString(id) {
			return nil, errors.New("invalid server")
		}
		name, ok := item["display_name"].(string)
		if !ok {
			name = "Server"
		}
		runes := []rune(name)
		if len(runes) > 160 {
			runes = runes[:160]
		}
		name = strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, string(runes))
		safe := map[string]any{"server_id": id, "display_name": name, "selected": item["selected"] == true}
		for _, field := range []string{"status", "ping_status", "availability"} {
			safe[field] = "untested"
			switch item[field] {
			case "ready", "failed", "untested":
				safe[field] = item[field]
			case "selected":
				if field == "status" {
					safe[field] = "ready"
				}
			}
		}
		for _, field := range []string{"latency_ms", "download_mbps", "download_seconds", "downloaded_bytes"} {
			if n, ok := item[field].(float64); ok && n >= 0 && !math.IsNaN(n) && !math.IsInf(n, 0) {
				safe[field] = n
			}
		}
		return safe, nil
	}
	if values, present := raw["servers"]; present {
		list, ok := values.([]any)
		if !ok || len(list) > 1000 {
			return nil, errors.New("invalid catalog")
		}
		safe := []any{}
		seen := map[string]bool{}
		for _, value := range list {
			item, err := row(value)
			if err != nil {
				return nil, err
			}
			id := item["server_id"].(string)
			if seen[id] {
				return nil, errors.New("duplicate server")
			}
			seen[id] = true
			safe = append(safe, item)
		}
		if ids != nil {
			if len(list) != len(ids) {
				return nil, errors.New("incomplete batch")
			}
			for _, id := range ids {
				if !seen[id] {
					return nil, errors.New("foreign batch result")
				}
			}
		}
		result["servers"] = safe
	} else if ids != nil && result["status"] == "ok" {
		return nil, errors.New("missing batch result")
	}
	if value, present := raw["server"]; present {
		safe, err := row(value)
		if err != nil {
			return nil, err
		}
		result["server"] = safe
	}
	return result, nil
}

func (b *Bridge) captureChecks(ctx context.Context, args, ids []string, progress func(picker.CheckProgress) error) ([]byte, ProcessResult, bool) {
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	output := &boundedOutput{limit: 1048576, cancel: cancel}
	stream := picker.NewCheckStream(ids, output, func(p picker.CheckProgress) error {
		if ctx.Err() != nil || progress == nil {
			return nil
		}
		if err := progress(p); err != nil {
			cancel()
			return err
		}
		return nil
	})
	result := RunProcess(child, b.options.Executable, args, append(os.Environ(), "VPNKIT_TUI_SUPERVISED=1"), stream, nil, b.options.Grace)
	err := stream.Finish()
	data, overflow := output.result()
	// Cancellation keeps its classification even if the stream has no final row.
	return data, result, overflow || (err != nil && result.Reason == "ok")
}
