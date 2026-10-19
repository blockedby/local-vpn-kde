//go:build linux

// vpnkit-portable is a single-download Arch installer. Pacman owns every
// installed file and runs the native package transaction guards.
package main

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strconv"
	"strings"
)

//go:embed payload/*
var payload embed.FS

type manifest struct {
	Format       int      `json:"format"`
	Package      string   `json:"package"`
	SHA256       string   `json:"sha256"`
	Architecture string   `json:"architecture"`
	Revision     string   `json:"revision"`
	Dependencies []string `json:"dependencies"`
}

func loadPayload() (manifest, []byte, error) { return readPayload(payload) }

func readPayload(files fs.FS) (manifest, []byte, error) {
	var m manifest
	b, err := fs.ReadFile(files, "payload/manifest.json")
	if err != nil {
		return m, nil, errors.New("no payload: build with packaging/portable/build.py")
	}
	if err = json.Unmarshal(b, &m); err != nil {
		return m, nil, err
	}
	if m.Format != 1 || m.Package != "application.pkg.tar.zst" || m.Architecture != "amd64" {
		return m, nil, errors.New("unsupported payload manifest")
	}
	b, err = fs.ReadFile(files, "payload/"+m.Package)
	if err != nil {
		return m, nil, err
	}
	digest := sha256.Sum256(b)
	if hex.EncodeToString(digest[:]) != m.SHA256 {
		return m, nil, errors.New("payload checksum mismatch")
	}
	return m, b, nil
}

func archHost(data string) bool {
	for _, line := range strings.Split(data, "\n") {
		name, value, ok := strings.Cut(line, "=")
		if !ok || (name != "ID" && name != "ID_LIKE") {
			continue
		}
		for _, id := range strings.Fields(strings.Trim(value, "\"'")) {
			if id == "arch" {
				return true
			}
		}
	}
	return false
}

func install(m manifest, data []byte) error {
	if os.Geteuid() != 0 {
		return errors.New("install requires root; inspect the payload first with --inspect")
	}
	if runtime.GOARCH != m.Architecture {
		return errors.New("payload architecture does not match this computer")
	}
	release, err := os.ReadFile("/etc/os-release")
	if err != nil || !archHost(string(release)) {
		return errors.New("this installer supports Arch Linux and Arch derivatives only")
	}
	caller, err := installationCaller(os.Getenv("SUDO_UID"), os.Getenv("PKEXEC_UID"), user.LookupId)
	if err != nil {
		return err
	}
	// Use a private directory under the root-owned /run, never a user-selected
	// TMPDIR. Pacman handles dependency checking, locks, ownership and rollback
	// markers. Package signature policy remains pacman's policy.
	dir, err := os.MkdirTemp("/run", "local-vpn-kde-payload-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := dir + "/" + m.Package
	if err = os.WriteFile(path, data, 0600); err != nil {
		return err
	}
	// Keep pacman's interactive confirmation for both transactions. Do not
	// refresh repositories alone (-Sy), disable signature checks or retry.
	commands := [][]string{}
	if len(m.Dependencies) > 0 {
		commands = append(commands, append([]string{"-S", "--needed", "--"}, m.Dependencies...))
	}
	commands = append(commands, []string{"-U", "--", path})
	for _, args := range commands {
		cmd := exec.Command("/usr/bin/pacman", args...)
		cmd.Env = []string{"PATH=/usr/bin:/usr/sbin:/bin:/sbin", "LANG=C.UTF-8", "HOME=/root"}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err = cmd.Run(); err != nil {
			return err
		}
	}
	if caller != "" {
		cmd := exec.Command("/usr/bin/usermod", "--append", "--groups", "local-vpn-kde-users", "--", caller)
		cmd.Env = []string{"PATH=/usr/bin:/usr/sbin:/bin:/sbin", "LANG=C.UTF-8"}
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("package installed, user authorization failed: %w", err)
		}
	}
	return nil
}

// Root elevation authenticates the installation caller once. Never infer an
// account from HOME/USER or enroll users during a generic pacman transaction.
func installationCaller(sudoUID, pkexecUID string, lookup func(string) (*user.User, error)) (string, error) {
	if sudoUID != "" && pkexecUID != "" && sudoUID != pkexecUID {
		return "", errors.New("ambiguous installation caller")
	}
	uidText := sudoUID
	if uidText == "" {
		uidText = pkexecUID
	}
	if uidText == "" {
		return "", nil
	} // direct root install: setup enrolls later
	uid, err := strconv.ParseUint(uidText, 10, 32)
	if err != nil || uid == 0 || strconv.FormatUint(uid, 10) != uidText {
		return "", errors.New("invalid installation caller")
	}
	account, err := lookup(uidText)
	if err != nil || account.Uid != uidText || account.Username == "" {
		return "", errors.New("installation caller unavailable")
	}
	return account.Username, nil
}

func run(args []string, out io.Writer) error {
	if len(args) != 1 || (args[0] != "--inspect" && args[0] != "--install") {
		return errors.New("usage: vpnkit-portable --inspect | --install (Arch x86_64; pacman confirms dependencies and package)")
	}
	m, data, err := loadPayload()
	if err != nil {
		return err
	}
	if args[0] == "--inspect" {
		return json.NewEncoder(out).Encode(m)
	}
	fmt.Fprintln(out, "Installing the embedded native package. Existing native package hooks guard upgrades; the invoking desktop user receives permission to manage this application without further password prompts. No VPN is connected by this installer.")
	return install(m, data)
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
