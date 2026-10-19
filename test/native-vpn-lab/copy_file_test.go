package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFixtureCopyLargeExecutableIsNotTruncated(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "binary")
	file, err := os.Create(source)
	if err != nil {
		t.Fatal(err)
	}
	const size = 65 << 20
	if err = file.Truncate(size); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if _, err = file.WriteAt([]byte("tail"), size-4); err != nil {
		file.Close()
		t.Fatal(err)
	}
	file.Close()
	target := filepath.Join(dir, "installed")
	if err = copyFile(source, target, 0555); err != nil {
		t.Fatal(err)
	}
	copied, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer copied.Close()
	stat, err := copied.Stat()
	if err != nil || stat.Size() != size {
		t.Fatal("executable was truncated")
	}
	tail := make([]byte, 4)
	if _, err = copied.ReadAt(tail, size-4); err != nil || string(tail) != "tail" {
		t.Fatal("executable tail was lost")
	}
	if err = copyFile(source, filepath.Join(dir, "data"), 0600); err == nil {
		t.Fatal("oversized non-executable accepted")
	}
	if _, err = os.Stat(filepath.Join(dir, "data")); !os.IsNotExist(err) {
		t.Fatal("oversized input created output")
	}
}
