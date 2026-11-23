//go:build !linux

package main

func effectiveSingBoxBin(p string) string {
	return p
}
