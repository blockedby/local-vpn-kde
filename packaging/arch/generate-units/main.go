//go:build linux

// generate-units materializes the canonical nativegateway systemd units into
// an explicit staging directory. It never writes to host system directories.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/blockedby/local-vpn-kde/internal/nativegateway"
)

func main() {
	if len(os.Args) != 2 || os.Args[1] == "" {
		fmt.Fprintln(os.Stderr, "usage: generate-units OUTPUT_DIRECTORY")
		os.Exit(2)
	}
	output := filepath.Clean(os.Args[1])
	info, err := os.Lstat(output)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		fmt.Fprintln(os.Stderr, "output must be an existing real directory")
		os.Exit(1)
	}

	units := nativegateway.UnitFiles()
	names := make([]string, 0, len(units))
	for name := range units {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if filepath.Base(name) != name {
			fmt.Fprintln(os.Stderr, "invalid unit name")
			os.Exit(1)
		}
		path := filepath.Join(output, name)
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				fmt.Fprintf(os.Stderr, "refusing to overwrite %s\n", path)
			} else {
				fmt.Fprintln(os.Stderr, "could not create staged unit")
			}
			os.Exit(1)
		}
		if _, err = file.WriteString(units[name]); err == nil {
			err = file.Sync()
		}
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			_ = os.Remove(path)
			fmt.Fprintln(os.Stderr, "could not write staged unit")
			os.Exit(1)
		}
	}
}
