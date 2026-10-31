package yamlconfig_test

import (
	"testing"

	"github.com/Djarvur/http-proxy-local/internal/out/yamlconfig"
)

// TestExampleConfigLoadsAndValidates proves the shipped config.example.yaml
// stays loadable and valid as fields are added. Every new config block must
// keep this test green — it is the contract between the example file and the
// loader.
func TestExampleConfigLoadsAndValidates(t *testing.T) {
	t.Parallel()

	const path = "../../../config.example.yaml"
	loader := yamlconfig.NewLoader()

	cfg, err := loader.Load(path)
	if err != nil {
		t.Fatalf("load config.example.yaml: %v", err)
	}
	if err := loader.Validate(cfg); err != nil {
		t.Fatalf("validate config.example.yaml: %v", err)
	}

	// The example ships with every opt-in listener commented out; the loaded
	// defaults must reflect the disabled posture.
	if cfg.Transparent.Listen != "" {
		t.Errorf("Transparent.Listen = %q, want empty (example ships disabled)", cfg.Transparent.Listen)
	}
	if cfg.SOCKS5.Listen != "" {
		t.Errorf("SOCKS5.Listen = %q, want empty (example ships disabled)", cfg.SOCKS5.Listen)
	}
}
