package main

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestModelRunPriorityFairnessIsOptInConfig(t *testing.T) {
	for _, tc := range []struct {
		yaml string
		want bool
	}{
		{"runtime: {}", false},
		{"runtime: {model_run_priority_fairness: false}", false},
		{"runtime: {model_run_priority_fairness: true}", true},
	} {
		var cfg config
		if err := yaml.Unmarshal([]byte(tc.yaml), &cfg); err != nil || cfg.Runtime.ModelRunPriorityFairness != tc.want {
			t.Fatalf("config %q: enabled=%v want=%v, %v", tc.yaml, cfg.Runtime.ModelRunPriorityFairness, tc.want, err)
		}
	}
}
