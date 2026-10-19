//go:build linux

package localvpn

import (
	"context"
	"errors"
	"os"

	"github.com/blockedby/local-vpn-kde/internal/nativegateway"
)

// InitializeNativeNamespace is a fixed root-only setup step executed by the
// installed network helper inside its owned namespace, before the restricted
// gateway service starts. Namespace deletion owns cleanup of these settings.
func InitializeNativeNamespace(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return errors.New("native namespace initialization requires root")
	}
	if err := initializeNativeNamespace(ctx, nativegateway.VerifyNamespace, nativeMutationCommands{}); err != nil {
		return err
	}
	return nativegateway.PublishNamespaceIdentity()
}

func initializeNativeNamespace(ctx context.Context, verify func() error, runner nativegateway.Runner) error {
	if err := verify(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return runner.Run(ctx, nativegateway.Command{Program: "sysctl", Args: []string{
		"-w",
		"net.ipv4.ip_forward=1",
		"net.ipv4.conf.all.src_valid_mark=1",
		"net.ipv4.conf.all.rp_filter=0",
		"net.ipv4.conf.default.rp_filter=0",
	}})
}
