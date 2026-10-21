//go:build linux

package localvpn

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/blockedby/local-vpn-kde/internal/nativegateway"
)

type namespaceRunner func(context.Context, nativegateway.Command) error

func (f namespaceRunner) Run(ctx context.Context, c nativegateway.Command) error { return f(ctx, c) }

func TestNativeNamespaceInitGuardFirst(t *testing.T) {
	called := false
	err := initializeNativeNamespace(context.Background(), func() error { return errors.New("wrong namespace") }, namespaceRunner(func(context.Context, nativegateway.Command) error { called = true; return nil }))
	if err == nil || called {
		t.Fatal("namespace failure reached mutation")
	}
}

func TestNativeNamespaceInitFixedSettings(t *testing.T) {
	verified := false
	err := initializeNativeNamespace(context.Background(), func() error { verified = true; return nil }, namespaceRunner(func(_ context.Context, c nativegateway.Command) error {
		want := []string{"-w", "net.ipv4.ip_forward=1", "net.ipv4.conf.all.src_valid_mark=1", "net.ipv4.conf.all.rp_filter=0", "net.ipv4.conf.default.rp_filter=0"}
		if !verified || c.Program != "sysctl" || !reflect.DeepEqual(c.Args, want) || c.Stdin != "" {
			t.Fatalf("unexpected namespace setup: %#v", c)
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
}
