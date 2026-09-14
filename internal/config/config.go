package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Duration struct{ time.Duration }

func NewDuration(d time.Duration) Duration      { return Duration{Duration: d} }
func (d Duration) String() string               { return d.Duration.String() }
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		return d.parse(s)
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("duration must be a string duration")
	}
	d.Duration = time.Duration(n)
	return nil
}
func (d *Duration) UnmarshalYAML(value *yaml.Node) error { return d.parse(value.Value) }
func (d *Duration) parse(s string) error {
	parsed, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return err
	}
	d.Duration = parsed
	return nil
}

type Config struct {
	SubscriptionFile   string `json:"subscription_file" yaml:"subscription_file"`
	Runtime            string `json:"runtime" yaml:"runtime"`
	SingBoxBin         string `json:"sing_box_bin" yaml:"sing_box_bin"`
	SingBoxConfig      string `json:"sing_box_config" yaml:"sing_box_config"`
	SingBoxService     string `json:"sing_box_service" yaml:"sing_box_service"`
	SingBoxRestartMode string `json:"sing_box_restart_mode" yaml:"sing_box_restart_mode"`
	SingBoxRestartFile string `json:"sing_box_restart_file" yaml:"sing_box_restart_file"`
	// SingBoxRestartAckFile names the supervisor health acknowledgement file.
	// Its token/generation/health fields are checked together by vibe-vpn.
	SingBoxRestartAckFile    string   `json:"sing_box_restart_ack_file" yaml:"sing_box_restart_ack_file"`
	SingBoxRestartAckTimeout Duration `json:"sing_box_restart_ack_timeout" yaml:"sing_box_restart_ack_timeout"`
	// SingBoxRestartAckGenerationFile names the monotonically increasing
	// runtime generation marker. It is intentionally separate from the health
	// acknowledgement file so a generation bump alone cannot commit state.
	SingBoxRestartAckGenerationFile string `json:"sing_box_restart_ack_generation_file,omitempty" yaml:"sing_box_restart_ack_generation_file,omitempty"`
	StateDir                        string `json:"state_dir" yaml:"state_dir"`
	ProductionSocks                 string `json:"production_socks" yaml:"production_socks"`
	TestSocks                       string `json:"test_socks" yaml:"test_socks"`
	TestURL                         string `json:"test_url" yaml:"test_url"`
	TestLimitKiB                    int    `json:"test_limit_kib" yaml:"test_limit_kib"`
	TestDurationSeconds             int    `json:"test_duration_seconds" yaml:"test_duration_seconds"`
	TimeoutSeconds                  int    `json:"timeout_seconds" yaml:"timeout_seconds"`
}

func Default() Config {
	return Config{
		SubscriptionFile:                "/etc/vibe-vpn/sub_url",
		Runtime:                         "singbox",
		SingBoxBin:                      "/usr/local/bin/sing-box",
		SingBoxConfig:                   "/var/lib/vpnkit/sing-box/config.json",
		SingBoxService:                  "vpnkit-supervised-sing-box",
		SingBoxRestartMode:              "request-file",
		SingBoxRestartFile:              "/run/vpnkit/restart-sing-box",
		SingBoxRestartAckGenerationFile: "/run/vpnkit/sing-box-generation",
		SingBoxRestartAckTimeout:        NewDuration(30 * time.Second),
		StateDir:                        "/var/lib/vibe-vpn",
		ProductionSocks:                 "127.0.0.1:2080",
		TestSocks:                       "127.0.0.1:18080",
		TestURL:                         "https://proof.ovh.net/files/10Mb.dat",
		TestLimitKiB:                    512,
		TimeoutSeconds:                  12,
	}
}

func Load(path string) (Config, error) {
	c := Default()
	if path == "" {
		return c, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, err
	}
	if strings.EqualFold(filepath.Ext(path), ".yaml") || strings.EqualFold(filepath.Ext(path), ".yml") {
		err = yaml.Unmarshal(b, &c)
	} else {
		err = json.Unmarshal(b, &c)
	}
	if err != nil {
		return c, err
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	if c.SubscriptionFile == "" {
		return fmt.Errorf("subscription_file is empty")
	}
	runtime := strings.ToLower(strings.TrimSpace(c.Runtime))
	if runtime == "" {
		runtime = "singbox"
	}
	switch runtime {
	case "singbox", "sing-box":
		if c.SingBoxBin == "" {
			return fmt.Errorf("sing_box_bin is empty")
		}
		if c.SingBoxConfig == "" {
			return fmt.Errorf("sing_box_config is empty")
		}
		if c.SingBoxService == "" {
			return fmt.Errorf("sing_box_service is empty")
		}
		if !strings.EqualFold(strings.TrimSpace(c.SingBoxRestartMode), "request-file") {
			return fmt.Errorf("local runtime requires request-file restart mode")
		}
		{
			if c.SingBoxRestartFile == "" {
				return fmt.Errorf("sing_box_restart_file is required for request-file mode")
			}
			if c.SingBoxRestartAckGenerationFile == "" {
				return fmt.Errorf("sing_box_restart_ack_generation_file is required for request-file mode")
			}
			// The runtime derives <generation>.ack when this optional explicit
			// path is omitted, but the generation marker itself is mandatory.
		}
	default:
		return fmt.Errorf("runtime must be singbox")
	}
	if c.StateDir == "" {
		return fmt.Errorf("state_dir is empty")
	}
	if c.ProductionSocks == "" {
		return fmt.Errorf("production_socks is empty")
	}
	if c.TestSocks == "" {
		return fmt.Errorf("test_socks is empty")
	}
	if c.TestURL == "" {
		return fmt.Errorf("test_url is empty")
	}
	if c.TestLimitKiB <= 0 {
		return fmt.Errorf("test_limit_kib must be positive")
	}
	if c.TestDurationSeconds < 0 {
		return fmt.Errorf("test_duration_seconds must be non-negative")
	}
	if c.TimeoutSeconds <= 0 {
		return fmt.Errorf("timeout_seconds must be positive")
	}
	if c.SingBoxRestartAckTimeout.Duration < 0 {
		return fmt.Errorf("sing_box_restart_ack_timeout must be non-negative")
	}
	return nil
}
