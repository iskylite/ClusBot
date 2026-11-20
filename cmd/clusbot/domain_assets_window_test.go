package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestImportDomainAssetsBenchmarkWindowYAML(t *testing.T) {
	stamp := time.Now().UnixMilli()
	start, end := stamp+60000, stamp+120000
	for _, tc := range []struct {
		name, startField string
		ok               bool
	}{
		{"positive_integer", fmt.Sprintf("valid_from_unix_ms: %d", start), true},
		{"legacy_omitted", "", true},
		{"null", "valid_from_unix_ms: null", false},
		{"blank", "valid_from_unix_ms:", false},
		{"zero", "valid_from_unix_ms: 0", false},
		{"float", fmt.Sprintf("valid_from_unix_ms: %d.5", start), false},
		{"string", fmt.Sprintf("valid_from_unix_ms: %q", fmt.Sprint(start)), false},
		{"same_as_end", fmt.Sprintf("valid_from_unix_ms: %d", end), false},
		{"unknown_field", fmt.Sprintf("window_starts_at: %d", start), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			file := filepath.Join(root, "assets.yaml")
			raw := fmt.Sprintf(`assets:
  - object_ref: node-1
    kind: node
    identity: local-fixture
    benchmark_grants:
      - revision: 1
        connection: bench
        tool: test
        allowed: {}
        expires_at_unix_ms: %d
        %s
`, end, tc.startField)
			if err := os.WriteFile(file, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if err := runImportDomainAssets([]string{"--file", file, "--data-dir", root}); (err == nil) != tc.ok {
				t.Fatalf("got %v, expected success=%t", err, tc.ok)
			}
		})
	}
}
