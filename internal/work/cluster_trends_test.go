package work

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClusterTrendStatsHistoryFaultsAndAuthorization(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "trends.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	stamp := func(day, hour int) int64 { return time.Date(2026, 11, day, hour, 0, 0, 0, time.UTC).UnixMilli() }
	now := time.UnixMilli(stamp(22, 12))
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	asset := func(ref, kind, state string, created, updated int64) {
		exec(`INSERT INTO ops_assets(object_ref,kind,identity,state,created_at,updated_at) VALUES(?,?,?,?,?,?)`, ref, kind, ref, state, created, updated)
	}
	auth := func(ref string, revoked int64) {
		exec(`INSERT INTO authorizations(auth_id,subject,action,object_ref,valid_from,revoked_at,owner,created_at,updated_at) VALUES(?,?, 'read',?,?,?,'owner',?,?)`, "auth-"+ref, "user:test", ref, stamp(1, 0), revoked, stamp(1, 0), stamp(1, 0))
	}
	history := func(ref, state string, at int64) {
		exec(`INSERT INTO asset_state_history(object_ref,state,revision,changed_by,occurred_at) VALUES(?,?,1,'owner',?)`, ref, state, at)
	}
	transition := func(ref, id, from, to string, at int64) {
		exec(`INSERT INTO asset_transitions(transition_id,object_ref,from_state,to_state,trigger_kind,expected_revision,expected_ownership_epoch,actor,occurred_at,created_at) VALUES(?,?,?,?,'test',1,1,'owner',?,?)`, id, ref, from, to, at, at)
	}
	asset("node-a", "node", AssetStateInService, stamp(15, 0), stamp(21, 9))
	history("node-a", AssetStateInService, stamp(15, 0))
	transition("node-a", "a-1", AssetStateInService, AssetStateMaintenance, stamp(19, 10))
	transition("node-a", "a-2", AssetStateMaintenance, AssetStateInService, stamp(21, 9))
	auth("node-a", 0)
	asset("node-b", "GPU 节点", AssetStateRecheck, stamp(19, 0), stamp(21, 8))
	history("node-b", AssetStateInService, stamp(19, 0))
	auth("node-b", 0)
	exec(`INSERT INTO loan_handovers(handover_id,object_ref,intent,ownership_epoch_before,state,created_at,updated_at) VALUES('handover','node-b','lend',1,'confirmed',?,?)`, stamp(20, 8), stamp(20, 8))
	exec(`INSERT INTO loans(loan_id,object_ref,handover_id,lender,borrower,state,ownership_epoch_at_lend,lent_at,returned_at) VALUES('loan','node-b','handover','lender','borrower','returned',1,?,?)`, stamp(20, 8), stamp(21, 8))
	asset("node-c", "计算节点", AssetStateInService, stamp(14, 0), stamp(21, 10))
	history("node-c", AssetStateMaintenance, stamp(14, 0))
	auth("node-c", 0)
	asset("node-revoked", "node", AssetStateInService, stamp(14, 0), stamp(14, 0))
	auth("node-revoked", stamp(18, 0))
	asset("node-private", "node", AssetStateInService, stamp(14, 0), stamp(14, 0))
	asset("gpu-a", "gpu", AssetStateInService, stamp(14, 0), stamp(14, 0))
	auth("gpu-a", 0)
	fault := func(id, key string, at int64) {
		exec(`INSERT INTO fault_segments(segment_id,fault_key,window_start,window_end,created_at,updated_at) VALUES(?,?,?,?,?,?)`, id, key, at, at, at, at)
	}
	fault("a", "src:node-a:network", stamp(17, 3))
	fault("b", "src:node-b:oob_alert", stamp(20, 3))
	fault("c", "src:node-a:custom", stamp(20, 8))
	fault("boundary", "src:node-a:nhc", stamp(21, 0))
	fault("private", "src:node-private:service", stamp(20, 3))
	fault("revoked", "src:node-revoked:network", stamp(20, 3))
	fault("ambiguous", "prefix:source:node-a:network", stamp(20, 3))
	fault("legacy", "node-a/nic.error", stamp(20, 3))
	fault("future", "src:node-a:network", stamp(22, 13))
	fault("before-node", "src:node-b:network", stamp(17, 3))

	got, err := store.ClusterTrendStats(ctx, "user:test", "7d", now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Range != "7d" || got.Timezone != "UTC" || got.BucketUnit != "day" || len(got.Buckets) != 7 || got.FaultCategoryBasis != "observation_kind" || got.StateHistoryBasis != "recorded_events_may_be_incomplete" {
		t.Fatalf("metadata: %+v", got)
	}
	assertBucket := func(i, total, service, unknown int, percent *float64) {
		t.Helper()
		b := got.Buckets[i]
		if b.TotalNodes != total || b.InServiceNodes != service || b.UnknownNodes != unknown {
			t.Fatalf("bucket %d: %+v", i, b)
		}
		if percent == nil && b.InServicePercent != nil || percent != nil && (b.InServicePercent == nil || *b.InServicePercent != *percent) {
			t.Fatalf("bucket %d percentage: %v, want %v", i, b.InServicePercent, percent)
		}
	}
	assertBucket(0, 2, 1, 1, nil) // Nov 16; node-c has an unrecorded change
	assertBucket(3, 3, 1, 1, nil) // Nov 19; node-a is in maintenance
	assertBucket(4, 3, 0, 1, nil) // Nov 20; node-b is lent
	percentage := 200.0 / 3.0
	assertBucket(5, 3, 2, 0, &percentage)
	assertBucket(6, 3, 2, 0, &percentage)
	if got.Buckets[1].FaultCounts["network"] != 1 || got.Buckets[4].FaultCounts["oob_alert"] != 1 || got.Buckets[4].FaultCounts["other"] != 1 || got.Buckets[5].FaultCounts["nhc"] != 1 {
		t.Fatalf("fault counts: %+v", got.Buckets)
	}
	faultCount := 0
	for _, bucket := range got.Buckets {
		for _, n := range bucket.FaultCounts {
			faultCount += n
		}
	}
	if faultCount != 4 {
		t.Fatalf("unauthorized or unattributable faults included: total %d", faultCount)
	}
	for _, tc := range []struct {
		window string
		n      int
		unit   string
	}{{"24h", 24, "hour"}, {"30d", 30, "day"}} {
		result, err := store.ClusterTrendStats(ctx, "user:test", tc.window, now)
		if err != nil || len(result.Buckets) != tc.n || result.BucketUnit != tc.unit {
			t.Fatalf("%s: %+v, %v", tc.window, result, err)
		}
	}
}

func TestClusterTrendStatsEmptyAndConflictingHistory(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "trends.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	out, err := store.ClusterTrendStats(context.Background(), "user:none", "7d", now)
	if err != nil || len(out.Buckets) != 7 || out.Buckets[6].InServicePercent != nil || out.Buckets[6].FaultCounts["network"] != 0 {
		t.Fatalf("empty: %+v err=%v", out, err)
	}
	if _, err := store.ClusterTrendStats(context.Background(), "", "7d", now); err == nil {
		t.Fatal("empty principal accepted")
	}
	if _, err := store.ClusterTrendStats(context.Background(), "user:none", "1y", now); err == nil {
		t.Fatal("unsupported range accepted")
	}

	// Conflicting records at the same instant have no reliable ordering. The
	// current asset row resolves only the current instant, not earlier buckets.
	created, conflict, updated := now.Add(-72*time.Hour).UnixMilli(), now.Add(-48*time.Hour).UnixMilli(), now.UnixMilli()
	for _, q := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO ops_assets(object_ref,kind,identity,state,created_at,updated_at) VALUES('n','node','n','在役',?,?)`, []any{created, updated}},
		{`INSERT INTO authorizations(auth_id,subject,action,object_ref,valid_from,owner,created_at,updated_at) VALUES('a','user:test','read','n',0,'owner',0,0)`, nil},
		{`INSERT INTO asset_state_history(object_ref,state,revision,changed_by,occurred_at) VALUES('n','在役',1,'owner',?)`, []any{conflict}},
		{`INSERT INTO asset_state_history(object_ref,state,revision,changed_by,occurred_at) VALUES('n','维护中',2,'owner',?)`, []any{conflict}},
	} {
		if _, err := store.db.Exec(q.query, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	out, err = store.ClusterTrendStats(context.Background(), "user:test", "7d", now)
	if err != nil {
		t.Fatal(err)
	}
	if out.Buckets[4].UnknownNodes != 1 || out.Buckets[4].InServicePercent != nil || out.Buckets[6].InServicePercent == nil || *out.Buckets[6].InServicePercent != 100 {
		t.Fatalf("conflicting history: %+v", out.Buckets)
	}
	out, err = store.ClusterTrendStats(context.Background(), "user:test", "24h", now)
	if err != nil || out.Buckets[0].UnknownNodes != 1 || out.Buckets[0].InServicePercent != nil {
		t.Fatalf("conflicting baseline before interval: %+v err=%v", out.Buckets, err)
	}
}

func TestClusterTrendFaultObservationAttribution(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "faults.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	stamp := now.Add(-time.Hour).UnixMilli()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, ref := range []string{"node:discovered:owned", "node-legacy", "node:discovered:private"} {
		exec(`INSERT INTO ops_assets(object_ref,kind,identity,state,created_at,updated_at) VALUES(?,'node',?,'在役',?,?)`, ref, ref, stamp-1000, stamp-1000)
	}
	for _, ref := range []string{"node:discovered:owned", "node-legacy"} {
		exec(`INSERT INTO authorizations(auth_id,subject,action,object_ref,valid_from,owner,created_at,updated_at) VALUES(?,'user:test','read',?,0,'owner',0,0)`, "auth-"+ref, ref)
	}
	for _, obs := range []struct{ id, object string }{
		{"obs-owned", "node:discovered:owned"},
		{"obs-legacy", "node-legacy"},
		{"obs-private", "node:discovered:private"},
	} {
		exec(`INSERT INTO observations(observation_id,source_ref,source_identity,collected_at,object_ref,field_key,value,recorded_at) VALUES(?,'source','instance',?,?,'nic.err.packets','1',?)`, obs.id, stamp, obs.object, stamp)
	}
	fault := func(id, key, refs string) {
		exec(`INSERT INTO fault_segments(segment_id,fault_key,observation_refs,window_start,window_end,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, id, key, refs, stamp, stamp, stamp, stamp)
	}
	fault("owned", "source:node:discovered:owned:network", `["obs-owned"]`)
	fault("colon-source", "collector:region1:node:discovered:owned:service", `["obs-owned"]`)
	fault("legacy", "node-legacy/nic.err.packets", `["obs-legacy"]`)
	fault("legacy-colon", "node:discovered:owned/nic.err.packets", `["obs-owned"]`)
	fault("private", "source:node:discovered:private:network", `["obs-private"]`)
	fault("mixed-nodes", "source:node:discovered:owned:network", `["obs-owned","obs-private"]`)
	fault("missing-ref", "source:node:discovered:owned:network", `["obs-owned","missing"]`)
	fault("mismatch", "source:node:discovered:private:network", `["obs-owned"]`)
	fault("ambiguous-key", "source:node:discovered:owned:network", `[]`)
	fault("ambiguous-digest", "source:node:discovered:owned:network", `["unknown-digest"]`)
	fault("legacy-mismatch", "node-private/nic.err.packets", `["obs-legacy"]`)
	fault("unlinked-slash", "node-legacy/nic.err.packets", `[]`)

	out, err := store.ClusterTrendStats(context.Background(), "user:test", "7d", now)
	if err != nil {
		t.Fatal(err)
	}
	counts := out.Buckets[len(out.Buckets)-1].FaultCounts
	if counts["network"] != 1 || counts["service"] != 1 || counts["other"] != 2 || counts["nhc"] != 0 || counts["oob_alert"] != 0 {
		t.Fatalf("faults included unauthorized or ambiguous records: %+v", counts)
	}
	if out.FaultCoverage != "consistent_observation_refs_or_simple_keys" {
		t.Fatalf("coverage omitted: %+v", out)
	}
}

func TestClusterTrendIndexMigration(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "migration.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for range 2 {
		if err := migrateClusterTrendIndex(tx); err != nil {
			t.Fatal(err)
		}
	}
	var definition string
	if err := tx.QueryRow(`SELECT sql FROM sqlite_master WHERE type='index' AND name='fault_segments_window_start'`).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(definition, "window_start,segment_id") {
		t.Fatalf("unexpected index definition: %s", definition)
	}
}

func TestClusterTrendIndexUpgradeFromPriorSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prior.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP INDEX IF EXISTS fault_segments_window_start`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`PRAGMA user_version=131`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND name='fault_segments_window_start'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("upgrade failed: index count %d, %v", count, err)
	}
}
