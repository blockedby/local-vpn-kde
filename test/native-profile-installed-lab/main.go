//go:build linux

// The native profile installation lab is test-only. It exercises installed
// native configuration generation in a disposable Arch container and leaves
// profile publication to the separate unprivileged transaction driver.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"

	"github.com/blockedby/local-vpn-kde/internal/localvpn"
)

const (
	labOptIn          = "VPNKIT_NATIVE_PACKAGE_LAB"
	fixtureDirectory  = "/work/native-profile-fixtures"
	subscriptionURL   = "https://fixture.invalid/subscription"
	builderAccount    = "builder"
	generationTimeout = 60 * time.Second
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "native_profile_installed_lab=error")
		os.Exit(1)
	}
	fmt.Println("native_profile_installed_lab=pass")
}

func run() error {
	if err := requireDisposableLab(); err != nil {
		return err
	}
	builderUID, err := lookupBuilderUID()
	if err != nil {
		return err
	}

	first, err := generateProfile(builderUID)
	if err != nil {
		return err
	}
	second, err := generateProfile(builderUID)
	if err != nil {
		return err
	}
	if len(first) == 0 || len(second) == 0 || string(first) == string(second) {
		return fmt.Errorf("native profile generations were not distinct")
	}
	return writeProfileFixtures(first, second)
}

func requireDisposableLab() error {
	if os.Getenv(labOptIn) != "1" {
		return fmt.Errorf("lab opt-in is required")
	}
	if _, err := os.Stat("/.dockerenv"); err != nil {
		return fmt.Errorf("Docker container marker is absent")
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("lab fixture requires root")
	}
	return nil
}

func lookupBuilderUID() (uint32, error) {
	account, err := user.Lookup(builderAccount)
	if err != nil {
		return 0, fmt.Errorf("desktop builder account is unavailable")
	}
	parsed, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil || parsed == 0 {
		return 0, fmt.Errorf("desktop builder UID is invalid")
	}
	return uint32(parsed), nil
}

func generateProfile(builderUID uint32) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), generationTimeout)
	defer cancel()
	generated, err := localvpn.GenerateNativeConfiguration(ctx, subscriptionURL, builderUID)
	if err != nil {
		return nil, fmt.Errorf("native profile generation failed")
	}
	return append([]byte(nil), generated.ClientProfile...), nil
}

func writeProfileFixtures(first, second []byte) error {
	if err := os.Mkdir(fixtureDirectory, 0700); err != nil {
		return fmt.Errorf("native profile fixture directory creation failed")
	}
	if err := os.Chmod(fixtureDirectory, 0700); err != nil {
		return fmt.Errorf("native profile fixture directory protection failed")
	}
	info, err := os.Lstat(fixtureDirectory)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return fmt.Errorf("native profile fixture directory is unsafe")
	}
	if err := writePrivateProfile(filepath.Join(fixtureDirectory, "first.ovpn"), first); err != nil {
		return err
	}
	if err := writePrivateProfile(filepath.Join(fixtureDirectory, "second.ovpn"), second); err != nil {
		return err
	}
	return nil
}

func writePrivateProfile(path string, profile []byte) (err error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("native profile fixture creation failed")
	}
	defer func() {
		closeErr := file.Close()
		if err == nil && closeErr != nil {
			err = fmt.Errorf("native profile fixture close failed")
		}
	}()
	if err := writeAll(file, profile); err != nil {
		return fmt.Errorf("native profile fixture write failed")
	}
	if err := file.Chmod(0600); err != nil {
		return fmt.Errorf("native profile fixture protection failed")
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("native profile fixture sync failed")
	}
	return nil
}

func writeAll(file *os.File, data []byte) error {
	for len(data) > 0 {
		written, err := file.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}
