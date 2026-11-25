package nativeupdate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const PackageName = "local-vpn-kde-native"
const OwnedFile = "/usr/libexec/local-vpn-kde/local-vpn-kde.bin"

// Runner runs an absolute-path command and returns its standard output.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

const maxCommandOutput = 64 * 1024

// ExecRunner runs a command with a fixed environment and bounded time and output.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	if !filepath.IsAbs(name) {
		return nil, fmt.Errorf("command path is not absolute: %q", name)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = []string{"LC_ALL=C", "PATH=/usr/bin:/bin"}
	var out limitedOutput
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// limitedOutput keeps its buffer in a named field so os/exec cannot bypass
// Write through bytes.Buffer.ReadFrom.
type limitedOutput struct{ buf bytes.Buffer }

func (o *limitedOutput) Write(p []byte) (int, error) {
	if len(p) > maxCommandOutput-o.buf.Len() {
		return 0, errors.New("command output exceeds 64 KiB")
	}
	return o.buf.Write(p)
}

func (o *limitedOutput) Bytes() []byte { return o.buf.Bytes() }

// Installation describes the native package that owns the installed executable.
type Installation struct {
	Manager             string
	Raw                 string
	Major, Minor, Patch int
	Pkgrel              int
}

var ErrNotPackaged = errors.New("kvpn is not installed from a native package")
var ErrDevelopmentBuild = errors.New("snapshot/dev versions are never auto-updated")

// DetectInstallation checks ownership with each installed package manager.
func DetectInstallation(ctx context.Context, run Runner, exists func(path string) bool) (Installation, error) {
	type candidate struct{ manager, raw string }
	var candidates []candidate
	if exists("/usr/bin/pacman") {
		owner, err := run(ctx, "/usr/bin/pacman", "-Qqo", OwnedFile)
		if err == nil {
			name := strings.TrimSpace(string(owner))
			if !validPackageName(name) {
				return Installation{}, fmt.Errorf("unexpected pacman ownership output: %q", owner)
			}
			if name == PackageName {
				output, err := run(ctx, "/usr/bin/pacman", "-Q", PackageName)
				if err == nil {
					line := strings.TrimSuffix(string(output), "\n")
					prefix := PackageName + " "
					if !strings.HasPrefix(line, prefix) || strings.TrimSpace(strings.TrimPrefix(line, prefix)) != strings.TrimPrefix(line, prefix) || strings.ContainsAny(strings.TrimPrefix(line, prefix), " \t\r\n") || len(line) == len(prefix) {
						return Installation{}, fmt.Errorf("unexpected pacman version output: %q", output)
					}
					candidates = append(candidates, candidate{"pacman", strings.TrimPrefix(line, prefix)})
				}
			}
		}
	}
	if exists("/usr/bin/dpkg-query") {
		owner, err := run(ctx, "/usr/bin/dpkg-query", "-S", OwnedFile)
		if err == nil {
			line := strings.TrimSuffix(string(owner), "\n")
			suffix := ": " + OwnedFile
			if !strings.HasSuffix(line, suffix) || !validPackageName(strings.TrimSuffix(line, suffix)) {
				return Installation{}, fmt.Errorf("unexpected dpkg ownership output: %q", owner)
			}
			if line == PackageName+suffix {
				output, err := run(ctx, "/usr/bin/dpkg-query", "-W", "-f=${Version}", PackageName)
				if err == nil {
					candidates = append(candidates, candidate{"dpkg", string(output)})
				}
			}
		}
	}
	if len(candidates) == 0 {
		return Installation{}, ErrNotPackaged
	}
	if len(candidates) > 1 {
		return Installation{}, errors.New("ambiguous package ownership")
	}
	c := candidates[0]
	result := Installation{Manager: c.manager, Raw: c.raw}
	version := c.raw
	var err error
	if c.manager == "pacman" {
		var release string
		version, release, _ = strings.Cut(c.raw, "-")
		result.Pkgrel, err = parseNumber(release)
		if err != nil || result.Pkgrel == 0 {
			return Installation{}, fmt.Errorf("%w: %q", ErrDevelopmentBuild, c.raw)
		}
	}
	result.Major, result.Minor, result.Patch, err = parseStable(version)
	if err != nil {
		return Installation{}, fmt.Errorf("%w: %q", ErrDevelopmentBuild, c.raw)
	}
	return result, nil
}

func validPackageName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("@._+-", c)) {
			return false
		}
	}
	return true
}

func parseNumber(s string) (int, error) {
	if s == "" || len(s) > 1 && s[0] == '0' {
		return 0, errors.New("invalid number")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("invalid number")
		}
	}
	return strconv.Atoi(s)
}

func parseStable(s string) (major, minor, patch int, err error) {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return 0, 0, 0, errors.New("invalid stable version")
	}
	if major, err = parseNumber(parts[0]); err != nil {
		return
	}
	if minor, err = parseNumber(parts[1]); err != nil {
		return
	}
	patch, err = parseNumber(parts[2])
	return
}

// Older reports whether the installed upstream version is strictly lower.
func (i Installation) Older(major, minor, patch int) bool {
	if i.Major != major {
		return i.Major < major
	}
	if i.Minor != minor {
		return i.Minor < minor
	}
	return i.Patch < patch
}
