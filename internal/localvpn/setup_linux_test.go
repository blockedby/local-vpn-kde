package localvpn

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupKeepsCommandOutputPrivate(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("normal-user installer")
	}
	repo := t.TempDir()
	dir := filepath.Join(repo, "scripts/vpnkit")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf 'vpnkit_phase=setup-profile\\nprivate-fixture-secret\\nNetworkManager profile import failed\\n'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "vpnkit-local-install.sh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := RunSetup(context.Background(), repo, "install", &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "private-fixture-secret") {
		t.Fatal("setup exposed private command output")
	}
	decoder := json.NewDecoder(&output)
	var progress struct{ Event, Phase string }
	var result struct {
		Event, Reason, Attempt string
		OK                     bool
	}
	if decoder.Decode(&progress) != nil || progress.Event != "progress" || progress.Phase != "setup-profile" {
		t.Fatal("missing setup progress")
	}
	if decoder.Decode(&result) != nil || result.Event != "result" || result.OK || result.Reason != "profile-import-failed" {
		t.Fatal("missing classified setup failure")
	}
	log := filepath.Join(repo, "secrets/vpnkit-local/diagnostics", result.Attempt+".log")
	data, err := os.ReadFile(log)
	if err != nil || !bytes.Contains(data, []byte("private-fixture-secret")) {
		t.Fatal("private diagnostic evidence missing")
	}
	info, err := os.Stat(log)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("setup log is not private")
	}
}
