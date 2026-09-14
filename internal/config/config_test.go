package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadRestartAcknowledgementSettings(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	body := []byte(`sing_box_restart_mode: request-file
sing_box_restart_file: /run/vpnkit/restart-sing-box
sing_box_restart_ack_generation_file: /run/vpnkit/sing-box-generation
sing_box_restart_ack_timeout: 17s
`)
	if err := os.WriteFile(p, body, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.SingBoxRestartAckGenerationFile != "/run/vpnkit/sing-box-generation" || c.SingBoxRestartAckTimeout.Duration != 17*time.Second {
		t.Fatalf("restart acknowledgement settings not loaded: %+v", c)
	}
}

func TestLoadRestartHealthAckRequiresGeneration(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	body := []byte(`sing_box_restart_mode: request-file
sing_box_restart_file: /run/vpnkit/restart-sing-box
sing_box_restart_ack_generation_file: ""
sing_box_restart_ack_file: /run/vpnkit/sing-box-generation.ack
`)
	if err := os.WriteFile(p, body, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("expected health acknowledgement without generation to fail validation")
	}
}

func TestLoadRequestFileRequiresGenerationEvenWithoutExplicitHealthAck(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	body := []byte(`sing_box_restart_mode: request-file
sing_box_restart_file: /run/vpnkit/restart-sing-box
sing_box_restart_ack_generation_file: ""
`)
	if err := os.WriteFile(p, body, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("expected request-file without generation acknowledgement to fail validation")
	}
}

func TestLoadRestartAcknowledgementTimeoutRejectsNegative(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"sing_box_restart_ack_timeout":"-1s"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("expected negative acknowledgement timeout to fail validation")
	}
}

func TestLoadSnakeCaseOverridesDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"subscription_file":"/tmp/sub","test_limit_kib":128,"timeout_seconds":5}`), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.SubscriptionFile != "/tmp/sub" || c.TestLimitKiB != 128 || c.TimeoutSeconds != 5 {
		t.Fatalf("unexpected overrides: %+v", c)
	}
	if c.SingBoxConfig == "" || c.TestSocks == "" {
		t.Fatalf("defaults not preserved: %+v", c)
	}
}

func TestLoadRejectsUnsafeEmptyPath(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"runtime":"xray","sing_box_config":""}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("expected validation error")
	}
}
