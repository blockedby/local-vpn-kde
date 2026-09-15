package localvpn

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// ExecUI validates the installed frontend and replaces this launcher with Bun.
// The frontend starts the native JSON bridge from the same checkout.
func ExecUI(repo string, args []string) error {
	if os.Geteuid() == 0 {
		return errors.New("run the interface as the desktop user")
	}
	for _, relative := range []string{"run.sh", ".build/local-vpn-kde.bin", "scripts/vpnkit/tui/index.ts", "scripts/vpnkit/tui/app.ts", "scripts/vpnkit/tui/model.ts", "scripts/vpnkit/tui/bridge.ts"} {
		path := filepath.Join(repo, relative)
		dir, err := directory(filepath.Dir(path), false, false)
		if err != nil {
			return err
		}
		fd, err := unix.Openat(dir, filepath.Base(path), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
		unix.Close(dir)
		if err != nil {
			return err
		}
		st, err := privateInode(fd)
		unix.Close(fd)
		if err != nil {
			return err
		}
		if st.Mode&0022 != 0 {
			return errors.New("application source is writable by another user")
		}
	}
	if _, err := os.Stat(filepath.Join(repo, "scripts/vpnkit/tui/node_modules/@opentui/core")); err != nil {
		return errors.New("interface dependencies missing; run ./install.sh")
	}
	bun, err := exec.LookPath("bun")
	if err != nil {
		return errors.New("Bun is unavailable")
	}
	argv := append([]string{bun, filepath.Join(repo, "scripts/vpnkit/tui/index.ts")}, args...)
	return unix.Exec(bun, argv, os.Environ())
}
