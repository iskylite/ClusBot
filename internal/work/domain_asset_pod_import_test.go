package work

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestImportDomainAssetsExplicitPODMembership(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "pod-import.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	entries := []DomainAssetEntry{
		{ObjectRef: "node-a", Kind: "node", Identity: "serial-a", PodRef: "group-a", Read: true},
		{ObjectRef: "node-b", Kind: "GPU 节点", Identity: "serial-b", PodRef: "group-b", Read: true},
		{ObjectRef: "node-loose", Kind: "node", Identity: "serial-loose", Read: true},
		{ObjectRef: "group-a", Kind: "pod", Identity: "Group A", Read: true},
		{ObjectRef: "group-b", Kind: "pod", Identity: "Group B", Read: true},
	}
	for i := 0; i < 2; i++ {
		if err := store.ImportDomainAssets(ctx, entries); err != nil {
			t.Fatalf("import %d: %v", i, err)
		}
	}
	var relations int
	if err := store.db.QueryRow(`SELECT count(*) FROM relations WHERE relation_kind='physical_parent'`).Scan(&relations); err != nil || relations != 2 {
		t.Fatalf("reimport not idempotent: count=%d err=%v", relations, err)
	}
	page, err := store.ClusterInventory(ctx, "user:session", "", 1, time.Now())
	if err != nil || page.Summary.TotalNodes != 3 || page.Summary.TopologyUnknownNodes != 1 || len(page.Summary.Pods) != 2 || page.Summary.Health.Unknown != 3 {
		t.Fatalf("imported topology not visible: %+v err=%v", page.Summary, err)
	}
	// Omission is backward-compatible and does not erase a relationship.
	if err := store.ImportDomainAssets(ctx, []DomainAssetEntry{{ObjectRef: "node-a", Kind: "node", Identity: "serial-a"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM relations WHERE relation_kind='physical_parent'`).Scan(&relations); err != nil || relations != 2 {
		t.Fatalf("omission changed topology: count=%d err=%v", relations, err)
	}

	// A conflicting move rejects the whole manifest, including new assets.
	err = store.ImportDomainAssets(ctx, []DomainAssetEntry{
		{ObjectRef: "new-node", Kind: "node", Identity: "new-serial", PodRef: "group-b"},
		{ObjectRef: "node-a", Kind: "node", Identity: "serial-a", PodRef: "group-b"},
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting move accepted: %v", err)
	}
	var exists bool
	if err := store.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM ops_assets WHERE object_ref='new-node')`).Scan(&exists); err != nil || exists {
		t.Fatalf("failed import was not atomic: exists=%t err=%v", exists, err)
	}
	for _, entry := range []DomainAssetEntry{
		{ObjectRef: "missing", Kind: "node", Identity: "missing", PodRef: "absent"},
		{ObjectRef: "bad-parent", Kind: "node", Identity: "bad-parent", PodRef: "node-a"},
		{ObjectRef: "bad-child", Kind: "gpu", Identity: "bad-child", PodRef: "group-a"},
	} {
		if err := store.ImportDomainAssets(ctx, []DomainAssetEntry{entry}); err == nil {
			t.Fatalf("invalid POD reference accepted: %+v", entry)
		}
	}
}
