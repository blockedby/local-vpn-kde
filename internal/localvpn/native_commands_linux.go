//go:build linux

package localvpn

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/blockedby/local-vpn-kde/internal/nativegateway"
)

// Native command adapters are internal: only the fixed probe and regenerated
// transaction plans may supply commands. No CLI accepts command/path input.
type nativeReadCommands struct{}
type nativeMutationCommands struct{}

func (nativeReadCommands) Run(ctx context.Context, program string, args ...string) ([]byte, error) {
	return nativeCommand(ctx, nativegateway.Command{Program: program, Args: args})
}
func (nativeMutationCommands) Run(ctx context.Context, command nativegateway.Command) error {
	if os.Geteuid() != 0 {
		return errors.New("native network changes require the installed privileged service")
	}
	_, err := nativeCommand(ctx, command)
	return err
}

func nativeCommand(ctx context.Context, command nativegateway.Command) ([]byte, error) {
	executable, err := nativeSystemExecutable(command.Program)
	if err != nil {
		return nil, err
	}
	if len(command.Stdin) > 1<<20 {
		return nil, errors.New("native command input too large")
	}
	if command.Stdin != "" && (command.Program != "nft" || len(command.Args) != 2 || command.Args[0] != "-f" || command.Args[1] != "-") {
		return nil, errors.New("native command input is not permitted")
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output := &boundedOutput{limit: nativegateway.ProbeMaxOutputBytes, cancel: cancel}
	result := runProcessInput(bounded, executable, command.Args, []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C", "LANG=C"}, strings.NewReader(command.Stdin), output, io.Discard, time.Second, true)
	data, overflow := output.result()
	if result.Reason != "ok" || overflow {
		return nil, errors.New("native network command failed")
	}
	return data, nil
}

func nativeSystemExecutable(program string) (string, error) {
	switch program {
	case "ip", "nft", "iptables", "sysctl", "nmcli":
	default:
		return "", errors.New("native command is not allowlisted")
	}
	for _, dir := range []string{"/usr/sbin", "/usr/bin", "/sbin", "/bin"} {
		path, err := filepath.EvalSymlinks(filepath.Join(dir, program))
		if err != nil {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			continue
		}
		safe := true
		for current := path; ; current = filepath.Dir(current) {
			info, err := os.Stat(current)
			if err != nil {
				safe = false
				break
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 {
				safe = false
				break
			}
			if current == "/" {
				break
			}
		}
		if safe {
			return path, nil
		}
	}
	return "", errors.New("required trusted native network utility unavailable")
}
