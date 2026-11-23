package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"clusbot/internal/work"
)

func TestImportDomainAssetsPODYAML(t *testing.T) {
	root := t.TempDir()
	manifest := filepath.Join(root, "assets.yaml")
	contents := []byte(`assets:
  - object_ref: node-a
    kind: node
    identity: serial-a
    pod_ref: group-a
    allow_read: true
  - object_ref: group-a
    kind: pod
    identity: Group A
    allow_read: true
`)
	if err := os.WriteFile(manifest, contents, 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := runImportDomainAssets([]string{"--file", manifest, "--data-dir", root}); err != nil {
			t.Fatalf("import %d: %v", i, err)
		}
	}
	store, err := work.Open(filepath.Join(root, "clusbot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	page, err := store.ClusterInventory(context.Background(), "user:session", "", 24, time.Now())
	if err != nil || page.Summary.TotalNodes != 1 || len(page.Summary.Pods) != 1 || page.Summary.Pods[0].ObjectRef != "group-a" || page.Summary.Pods[0].TotalNodes != 1 {
		t.Fatalf("YAML import not reflected in inventory: %+v err=%v", page.Summary, err)
	}
}
