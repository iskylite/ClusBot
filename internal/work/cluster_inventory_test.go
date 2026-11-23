package work

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestClusterInventoryAuthorizationAndPagination(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "inventory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, asset := range []struct{ ref, kind string }{
		{"node-a", "node"}, {"node-b", "GPU 节点"}, {"node-private", "node"}, {"node-revoked", "node"}, {"gpu", "gpu"},
	} {
		exec(`INSERT INTO ops_assets(object_ref,kind,identity,state,created_at,updated_at) VALUES(?,?,?,'在役',1,1)`, asset.ref, asset.kind, asset.ref)
	}
	for _, grant := range []struct {
		ref     string
		revoked int
	}{
		{"node-a", 0}, {"node-b", 0}, {"node-revoked", 1}, {"gpu", 0},
	} {
		exec(`INSERT INTO authorizations(auth_id,subject,action,object_ref,valid_from,revoked_at,owner,created_at,updated_at) VALUES(?, 'user:test','read',?,0,?,'owner',0,0)`, "auth-"+grant.ref, grant.ref, grant.revoked)
	}
	first, err := store.ClusterInventory(context.Background(), "user:test", "", 1, now)
	if err != nil || len(first.Items) != 1 || first.Items[0].ObjectRef != "node-a" || first.NextCursor != "node-a" {
		t.Fatalf("first page: %+v err=%v", first, err)
	}
	second, err := store.ClusterInventory(context.Background(), "user:test", first.NextCursor, 1, now)
	if err != nil || len(second.Items) != 1 || second.Items[0].ObjectRef != "node-b" || second.NextCursor != "" {
		t.Fatalf("second page: %+v err=%v", second, err)
	}
	other, err := store.ClusterInventory(context.Background(), "user:other", "", 24, now)
	if err != nil || len(other.Items) != 0 {
		t.Fatalf("other principal: %+v err=%v", other, err)
	}
	if _, err := store.ClusterInventory(context.Background(), "", "", 24, now); err == nil {
		t.Fatal("empty principal accepted")
	}
	exec(`UPDATE authorizations SET revoked_at=1 WHERE object_ref='node-b'`)
	revoked, err := store.ClusterInventory(context.Background(), "user:test", "", 24, now)
	if err != nil || len(revoked.Items) != 1 || revoked.Items[0].ObjectRef != "node-a" {
		t.Fatalf("revocation: %+v err=%v", revoked, err)
	}
}

func TestClusterInventoryGlobalHealthAndExplicitTopology(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "global-inventory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	at := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	stamp := at.UnixMilli()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	asset := func(ref, kind string) {
		t.Helper()
		if _, err := upsertAsset(tx, OpsAsset{ObjectRef: ref, Identity: "name-" + ref, Kind: kind, State: AssetStateInService}, stamp-1000); err != nil {
			t.Fatal(err)
		}
	}
	grant := func(subject, ref string, expires int64) {
		t.Helper()
		if _, err := tx.Exec(`INSERT INTO authorizations(auth_id,subject,action,object_ref,valid_from,expires_at,revoked_at,owner,created_at,updated_at)
			VALUES(?,?,'read',?,0,?,0,'owner',0,0)`, subject+":"+ref, subject, ref, expires); err != nil {
			t.Fatal(err)
		}
	}
	relate := func(parent, child, kind string, validFrom, validTo int64) {
		t.Helper()
		if _, err := assertRelation(tx, Relation{RelationKind: kind, SourceRef: parent, TargetRef: child, ValidFrom: validFrom, ValidTo: validTo}, stamp-500); err != nil {
			t.Fatal(err)
		}
	}
	health := func(id, ref, status string, occurred int64) {
		t.Helper()
		if _, err := tx.Exec(`INSERT INTO asset_transitions(transition_id,object_ref,from_state,to_state,trigger_kind,expected_revision,expected_ownership_epoch,actor,health,occurred_at,created_at)
			VALUES(?,?,'在役','在役','record',1,1,'operator',?,?,?)`, id, ref, status, occurred, occurred); err != nil {
			t.Fatal(err)
		}
	}
	for _, pod := range []string{"pod-a", "pod-b", "pod-hidden"} {
		asset(pod, "pod")
	}
	for _, ref := range []string{"node-01", "node-02", "node-03", "node-04", "node-05", "node-06", "node-private", "node-expired"} {
		asset(ref, "node")
	}
	for _, ref := range []string{"pod-a", "pod-b", "node-01", "node-02", "node-03", "node-04", "node-05", "node-06"} {
		grant("user:first", ref, 0)
	}
	grant("user:first", "node-expired", stamp-1)
	grant("user:second", "node-private", 0)
	relate("pod-a", "node-01", "physical_parent", stamp-500, 0)
	relate("pod-a", "node-02", "physical_component", stamp-500, 0)
	relate("pod-b", "node-03", "physical_parent", stamp-500, 0)
	relate("pod-hidden", "node-05", "physical_parent", stamp-500, 0)
	// Distinct kinds can represent conflicting parents; never guess one.
	relate("pod-a", "node-06", "physical_parent", stamp-500, 0)
	relate("pod-b", "node-06", "physical_component", stamp-500, 0)
	relate("pod-a", "node-private", "physical_parent", stamp-500, 0)
	// An expired relationship and a future relationship do not establish membership.
	relate("pod-b", "node-04", "physical_parent", stamp-700, stamp-100)
	relate("pod-a", "node-04", "physical_component", stamp+100, 0)
	health("health-01", "node-01", "healthy", stamp-300)
	health("health-02-old", "node-02", "healthy", stamp-400)
	health("health-02-new", "node-02", "degraded", stamp-200)
	health("health-02-future", "node-02", "healthy", stamp+100)
	health("health-03", "node-03", "unhealthy", stamp-200)
	health("health-05", "node-05", "healthy", stamp-200)
	health("health-06", "node-06", "degraded", stamp-200)
	health("health-private", "node-private", "unhealthy", stamp-200)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	first, err := store.ClusterInventory(ctx, "user:first", "", 2, at)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.ClusterInventory(ctx, "user:first", first.NextCursor, 2, at)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 2 || first.NextCursor != "node-02" || len(second.Items) != 2 || second.Items[0].ObjectRef != "node-03" {
		t.Fatalf("pagination: first=%+v second=%+v", first, second)
	}
	if !reflect.DeepEqual(first.Summary, second.Summary) {
		t.Fatalf("summary changed with cursor: first=%+v second=%+v", first.Summary, second.Summary)
	}
	summary := first.Summary
	if summary.TotalNodes != 6 || summary.Health != (ClusterHealthCounts{Healthy: 2, Degraded: 2, Unhealthy: 1, Unknown: 1}) || summary.HealthPercent != nil || summary.TopologyUnknownNodes != 3 {
		t.Fatalf("wrong global summary: %+v", summary)
	}
	if len(summary.Pods) != 2 || summary.Pods[0].ObjectRef != "pod-a" || summary.Pods[0].TotalNodes != 2 || summary.Pods[0].Status != "degraded" || summary.Pods[1].ObjectRef != "pod-b" || summary.Pods[1].Status != "unhealthy" {
		t.Fatalf("wrong POD summary: %+v", summary.Pods)
	}
	if summary.ExceptionTotal != 4 || len(summary.Exceptions) != 4 || summary.Exceptions[0].ObjectRef != "node-03" || first.Items[1].Health != "degraded" || first.Items[1].HealthAt != stamp-200 {
		t.Fatalf("wrong recorded health or exception ordering: page=%+v summary=%+v", first.Items, summary)
	}
	if first.Items[0].PodRef != "pod-a" || first.Items[0].PodName != "name-pod-a" || first.Items[1].PodRef != "pod-a" || second.Items[1].PodRef != "" {
		t.Fatalf("node membership must use only readable, unambiguous relations: first=%+v second=%+v", first.Items, second.Items)
	}
	podFirst, err := store.ClusterInventoryForPod(ctx, "user:first", "pod-a", "", 1, at)
	if err != nil || len(podFirst.Items) != 1 || podFirst.Items[0].ObjectRef != "node-01" || podFirst.NextCursor != "node-01" || !reflect.DeepEqual(podFirst.Summary, summary) {
		t.Fatalf("first POD page: %+v err=%v", podFirst, err)
	}
	podSecond, err := store.ClusterInventoryForPod(ctx, "user:first", "pod-a", podFirst.NextCursor, 1, at)
	if err != nil || len(podSecond.Items) != 1 || podSecond.Items[0].ObjectRef != "node-02" || podSecond.NextCursor != "" || !reflect.DeepEqual(podSecond.Summary, summary) {
		t.Fatalf("second POD page: %+v err=%v", podSecond, err)
	}
	for _, ref := range []string{"pod-hidden", "missing-pod"} {
		if _, err := store.ClusterInventoryForPod(ctx, "user:first", ref, "", 24, at); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("inaccessible or missing POD %q: %v", ref, err)
		}
	}
	for _, exception := range summary.Exceptions {
		if exception.ObjectRef == "node-06" && exception.PodRef != "" {
			t.Fatalf("conflicting topology guessed: %+v", exception)
		}
	}
	other, err := store.ClusterInventory(ctx, "user:second", "", 2, at)
	if err != nil || other.Summary.TotalNodes != 1 || other.Summary.TopologyUnknownNodes != 1 || len(other.Summary.Pods) != 0 || other.Summary.HealthPercent == nil || *other.Summary.HealthPercent != 0 {
		t.Fatalf("authorization isolation: %+v err=%v", other, err)
	}
	if _, err := store.db.Exec(`UPDATE authorizations SET revoked_at=? WHERE subject='user:first' AND object_ref='node-02'`, stamp); err != nil {
		t.Fatal(err)
	}
	afterNodeRevocation, err := store.ClusterInventoryForPod(ctx, "user:first", "pod-a", "", 24, at)
	if err != nil || len(afterNodeRevocation.Items) != 1 || afterNodeRevocation.Items[0].ObjectRef != "node-01" || afterNodeRevocation.Summary.TotalNodes != 5 {
		t.Fatalf("revoked node remained visible: %+v err=%v", afterNodeRevocation, err)
	}
	if _, err := store.db.Exec(`UPDATE authorizations SET revoked_at=? WHERE subject='user:first' AND object_ref='pod-a'`, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClusterInventoryForPod(ctx, "user:first", "pod-a", "", 24, at); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("revoked POD remained visible: %v", err)
	}
	afterRevocation, err := store.ClusterInventory(ctx, "user:first", "", 2, at)
	if err != nil || afterRevocation.Items[0].PodRef != "" || afterRevocation.Items[0].PodName != "" {
		t.Fatalf("revoked parent leaked in node page: %+v err=%v", afterRevocation.Items, err)
	}
}
