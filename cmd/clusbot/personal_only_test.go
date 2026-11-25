package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemovedConfigurationFailsBeforeCreatingData(t *testing.T) {
	for _, tc := range []struct{ key, yaml string }{
		{"jobs", "jobs:\n  endpoint: http://127.0.0.1:8999\n"},
		{"monitoring", "monitoring:\n  target_config: cluster.yaml\n"},
		{"kubernetes_scale_config", "kubernetes_scale_config: cluster.yaml\n"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			base := t.TempDir()
			cfg, data := filepath.Join(base, "config.yaml"), filepath.Join(base, "data")
			if err := os.WriteFile(cfg, []byte(tc.yaml), 0600); err != nil {
				t.Fatal(err)
			}
			previous := os.Args
			os.Args = []string{"clusbot", "serve", "--config", cfg, "--data-dir", data}
			defer func() { os.Args = previous }()
			if err := run(); err == nil || !strings.Contains(err.Error(), "field "+tc.key+" not found") {
				t.Fatalf("removed configuration was not rejected: %v", err)
			}
			if _, err := os.Stat(data); !os.IsNotExist(err) {
				t.Fatalf("invalid configuration touched data: %v", err)
			}
		})
	}
}

func TestRemovedCommandIsNotAvailable(t *testing.T) {
	previous := os.Args
	os.Args = []string{"clusbot", "mcp-kubernetes"}
	defer func() { os.Args = previous }()
	if err := run(); err == nil || err.Error() != "无法识别的参数" {
		t.Fatalf("removed command: %v", err)
	}
}

func TestUnsupportedAccountProviderConfigurationsFailBeforeCreatingData(t *testing.T) {
	for _, tc := range []struct{ setting, yaml string }{
		{"tickets.", "tickets:\n  health_credential_env: LEGACY_HEALTH_KEY\n"},
		{"benchmark.event_secret_env", "benchmark:\n  event_secret_env: LEGACY_EVENT_KEY\n"},
	} {
		t.Run(tc.setting, func(t *testing.T) {
			base := t.TempDir()
			cfg, data := filepath.Join(base, "config.yaml"), filepath.Join(base, "data")
			if err := os.WriteFile(cfg, []byte(tc.yaml), 0600); err != nil {
				t.Fatal(err)
			}
			previous := os.Args
			os.Args = []string{"clusbot", "serve", "--config", cfg, "--data-dir", data}
			defer func() { os.Args = previous }()
			if err := run(); err == nil || !strings.Contains(err.Error(), tc.setting) {
				t.Fatalf("unsupported account provider setting was silently accepted: %v", err)
			}
			assertNoStartupPath(t, base, "data")
		})
	}
}
