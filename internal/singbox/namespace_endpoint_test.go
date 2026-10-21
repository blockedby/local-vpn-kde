package singbox

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPrepareOutboundForNamespacePrefersIPv4AndPreservesMetadata(t *testing.T) {
	originalLookup := namespaceLookupIP
	t.Cleanup(func() { namespaceLookupIP = originalLookup })
	namespaceLookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
		if host != "edge.example.test" {
			t.Fatalf("unexpected lookup host %q", host)
		}
		return []net.IP{
			net.ParseIP("2001:db8::20"),
			net.ParseIP("192.0.2.20"),
		}, nil
	}

	out := map[string]any{
		"type":   "vless",
		"server": "edge.example.test",
		"tls": map[string]any{
			"enabled":     true,
			"server_name": "reality.example.test",
			"reality": map[string]any{
				"enabled":    true,
				"public_key": "public-key",
				"short_id":   "short-id",
			},
		},
		"transport": map[string]any{
			"type": "ws",
			"path": "/socket",
			"headers": map[string]any{
				"Host": "ws.example.test",
			},
		},
	}
	wantInput := map[string]any{
		"type":   "vless",
		"server": "edge.example.test",
		"tls": map[string]any{
			"enabled":     true,
			"server_name": "reality.example.test",
			"reality": map[string]any{
				"enabled":    true,
				"public_key": "public-key",
				"short_id":   "short-id",
			},
		},
		"transport": map[string]any{
			"type": "ws",
			"path": "/socket",
			"headers": map[string]any{
				"Host": "ws.example.test",
			},
		},
	}

	got, err := PrepareOutboundForNamespace(context.Background(), out)
	if err != nil {
		t.Fatalf("PrepareOutboundForNamespace() error = %v", err)
	}
	if got["server"] != "192.0.2.20" {
		t.Fatalf("server = %v, want preferred IPv4", got["server"])
	}
	if !reflect.DeepEqual(out, wantInput) {
		t.Fatalf("input was mutated:\n got: %#v\nwant: %#v", out, wantInput)
	}
	if !reflect.DeepEqual(got["tls"], out["tls"]) || !reflect.DeepEqual(got["transport"], out["transport"]) {
		t.Fatalf("TLS or transport metadata changed: %#v", got)
	}
	got["tag"] = "prepared"
	if _, exists := out["tag"]; exists {
		t.Fatal("returned top-level map aliases the input")
	}
}

func TestPrepareOutboundForNamespaceIPv6FallbackMatchesContainer(t *testing.T) {
	addresses := []net.IP{net.ParseIP("2001:db8::42"), net.ParseIP("2001:db8::43")}
	originalNamespaceLookup := namespaceLookupIP
	originalLookup := lookupIP
	t.Cleanup(func() {
		namespaceLookupIP = originalNamespaceLookup
		lookupIP = originalLookup
	})
	namespaceLookupIP = func(context.Context, string) ([]net.IP, error) {
		return addresses, nil
	}
	lookupIP = func(string) ([]net.IP, error) {
		return addresses, nil
	}

	prepared, err := PrepareOutboundForNamespace(context.Background(), map[string]any{"server": "edge.example.test"})
	if err != nil {
		t.Fatalf("PrepareOutboundForNamespace() error = %v", err)
	}
	container, err := preResolveOutboundServer(map[string]any{"server": "edge.example.test"})
	if err != nil {
		t.Fatalf("preResolveOutboundServer() error = %v", err)
	}
	if prepared["server"] != "2001:db8::42" || prepared["server"] != container["server"] {
		t.Fatalf("namespace server = %v, container server = %v", prepared["server"], container["server"])
	}
}

func TestPrepareOutboundForNamespaceLiteralsDoNotResolve(t *testing.T) {
	originalLookup := namespaceLookupIP
	t.Cleanup(func() { namespaceLookupIP = originalLookup })
	namespaceLookupIP = func(context.Context, string) ([]net.IP, error) {
		t.Fatal("IP literal unexpectedly triggered DNS")
		return nil, nil
	}

	for _, literal := range []string{"192.0.2.8", "2001:0db8::8"} {
		t.Run(literal, func(t *testing.T) {
			out := map[string]any{"server": literal, "tls": map[string]any{"server_name": "edge.example.test"}}
			got, err := PrepareOutboundForNamespace(context.Background(), out)
			if err != nil {
				t.Fatalf("PrepareOutboundForNamespace() error = %v", err)
			}
			if got["server"] != literal {
				t.Fatalf("server = %v, want unchanged literal %q", got["server"], literal)
			}
			got["server"] = "changed"
			if out["server"] != literal {
				t.Fatal("input map was mutated")
			}
		})
	}
}

func TestPrepareOutboundForNamespaceUsesBoundedResolverContext(t *testing.T) {
	originalLookup := namespaceLookupIP
	t.Cleanup(func() { namespaceLookupIP = originalLookup })
	namespaceLookupIP = func(ctx context.Context, _ string) ([]net.IP, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("resolver context has no deadline")
		}
		remaining := time.Until(deadline)
		if remaining <= 0 || remaining > namespaceLookupTimeout {
			t.Fatalf("resolver deadline remaining = %v, want (0, %v]", remaining, namespaceLookupTimeout)
		}
		return []net.IP{net.ParseIP("192.0.2.9")}, nil
	}

	got, err := PrepareOutboundForNamespace(context.Background(), map[string]any{"server": "edge.example.test"})
	if err != nil {
		t.Fatalf("PrepareOutboundForNamespace() error = %v", err)
	}
	if got["server"] != "192.0.2.9" {
		t.Fatalf("server = %v", got["server"])
	}
}

func TestPrepareOutboundForNamespaceErrorsAreGeneric(t *testing.T) {
	t.Run("empty endpoint", func(t *testing.T) {
		got, err := PrepareOutboundForNamespace(context.Background(), map[string]any{"tls": map[string]any{"server_name": "private.example"}})
		assertGenericNamespaceError(t, got, err, "private.example")
	})

	t.Run("lookup failure", func(t *testing.T) {
		originalLookup := namespaceLookupIP
		t.Cleanup(func() { namespaceLookupIP = originalLookup })
		namespaceLookupIP = func(context.Context, string) ([]net.IP, error) {
			return nil, errors.New("secret resolver payload with private://link")
		}
		got, err := PrepareOutboundForNamespace(context.Background(), map[string]any{"server": "private-endpoint.example"})
		assertGenericNamespaceError(t, got, err, "private-endpoint.example", "secret resolver payload", "private://link")
	})

	t.Run("no addresses", func(t *testing.T) {
		originalLookup := namespaceLookupIP
		t.Cleanup(func() { namespaceLookupIP = originalLookup })
		namespaceLookupIP = func(context.Context, string) ([]net.IP, error) { return nil, nil }
		got, err := PrepareOutboundForNamespace(context.Background(), map[string]any{"server": "empty.example"})
		assertGenericNamespaceError(t, got, err, "empty.example")
	})
}

func TestPrepareOutboundForNamespaceHonorsCancellation(t *testing.T) {
	originalLookup := namespaceLookupIP
	t.Cleanup(func() { namespaceLookupIP = originalLookup })
	resolverCalled := make(chan struct{})
	namespaceLookupIP = func(ctx context.Context, _ string) ([]net.IP, error) {
		close(resolverCalled)
		<-ctx.Done()
		return nil, errors.New("private cancellation payload")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := PrepareOutboundForNamespace(ctx, map[string]any{"server": "cancel.example"})
		done <- err
	}()
	<-resolverCalled
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		if strings.Contains(err.Error(), "cancel.example") || strings.Contains(err.Error(), "private cancellation payload") {
			t.Fatalf("error leaks endpoint or resolver payload: %q", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation was not honored")
	}
}

func assertGenericNamespaceError(t *testing.T, got map[string]any, err error, forbidden ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	if got != nil {
		t.Fatalf("outbound = %#v, want nil on error", got)
	}
	if len(err.Error()) > 100 {
		t.Fatalf("error is unexpectedly unbounded: %q", err)
	}
	for _, value := range forbidden {
		if strings.Contains(err.Error(), value) {
			t.Fatalf("error leaks %q: %q", value, err)
		}
	}
}
