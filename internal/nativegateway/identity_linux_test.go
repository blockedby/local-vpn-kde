//go:build linux

package nativegateway

import (
	"golang.org/x/sys/unix"
	"testing"
)

func TestVerifyIdentity(t *testing.T) {
	host := unix.Stat_t{Dev: 1, Ino: 10}
	native := unix.Stat_t{Dev: 1, Ino: 20}
	foreign := unix.Stat_t{Dev: 2, Ino: 20}
	for _, tc := range []struct {
		name                     string
		target, current, initial unix.Stat_t
		ok                       bool
	}{
		{"native", native, native, host, true},
		{"host", native, host, host, false},
		{"aliased host mount", host, host, host, false},
		{"foreign namespace", native, foreign, host, false},
		{"wrong named mount", foreign, native, host, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := verifyIdentity(tc.target, tc.current, tc.initial); (err == nil) != tc.ok {
				t.Fatalf("verifyIdentity error=%v, want success=%v", err, tc.ok)
			}
		})
	}
}
