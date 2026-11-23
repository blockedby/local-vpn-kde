//go:build linux

package main

import "github.com/blockedby/local-vpn-kde/internal/nativegateway"

func effectiveSingBoxBin(p string) string {
	return nativegateway.EffectiveSingBoxBin(p)
}
