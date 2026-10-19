package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestNonApplyingBenchmarkCommand(t *testing.T) {
	root := newRootCommand()
	command, _, err := root.Find([]string{"test"})
	if err != nil || command == root || command.Name() != "test" {
		t.Fatal("non-applying benchmark command missing")
	}
	for _, name := range []string{"max", "limit-kib", "duration-sec", "default-exclude"} {
		if command.Flags().Lookup(name) == nil {
			t.Fatalf("missing benchmark flag %s", name)
		}
	}
	if command.Flags().Lookup("restart-async") != nil {
		t.Fatal("benchmark exposed restart option")
	}
	if err := command.Args(command, []string{"unexpected"}); err == nil {
		t.Fatal("unexpected arguments accepted")
	}
	output := new(bytes.Buffer)
	root.SetOut(output)
	root.SetErr(output)
	if err := executeRootCommand(root, []string{"test", "--help"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "without applying") {
		t.Fatal("non-applying help missing")
	}
}
