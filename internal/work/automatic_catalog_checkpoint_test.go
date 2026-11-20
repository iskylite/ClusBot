package work

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"clusbot/internal/mcpclient"
)

const catalogFixtureBinding = "automatic-catalog-checkpoint-fixture"

type automaticCatalogFixture struct {
	offers    [][]string
	interrupt bool
}

func (*automaticCatalogFixture) Stream(context.Context, []Message, bool, int, func(string) error) (*TokenUsage, error) {
	return nil, errors.New("expected automatic tool offer")
}

func (p *automaticCatalogFixture) TextRunProvider(context.Context, bool) (Provider, string, error) {
	return p, catalogFixtureBinding, nil
}

func (p *automaticCatalogFixture) TaskToolTurn(_ context.Context, _ []Message, tools []TaskToolDefinition, _ []TaskReadRound, _ int, emit func(string) error) (TaskToolTurn, error) {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	p.offers = append(p.offers, names)
	if p.interrupt {
		p.interrupt = false
		return TaskToolTurn{}, io.ErrUnexpectedEOF
	}
	return TaskToolTurn{}, emit("本机模拟结果")
}

func TestAutomaticCatalogCheckpointRejectsNewOfferAfterRecovery(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "automatic-catalog.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	const env = "CLUSBOT_AUTOCATALOG_FIXTURE_TOKEN"
	t.Setenv(env, "local-fixture")
	const toolName = "fixture_read"
	tool := mcpclient.Tool{Name: toolName, Version: digest(toolName), InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`)}
	definition, err := json.Marshal(tool)
	if err != nil {
		t.Fatal(err)
	}
	connection := func(name string, allowed bool) Connection {
		t.Helper()
		cfg := mcpclient.Config{Transport: "http", Endpoint: "http://127.0.0.1:1", TokenEnv: env}
		conn, err := store.SaveConnection(ctx, "", Connection{Name: name, Enabled: true, Config: cfg})
		if err != nil {
			t.Fatal(err)
		}
		fingerprint, err := cfg.CredentialBinding(store.connectionKey)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec("UPDATE connections SET status='ready',credential_binding=? WHERE id=?", fingerprint, conn.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec("INSERT INTO connection_tools VALUES(?,?,?,?)", conn.ID, toolName, string(definition), allowed); err != nil {
			t.Fatal(err)
		}
		return conn
	}
	store.SetConnectionCredentialKey("stable-automatic-catalog-fixture")
	first, extra := connection("A-existing", true), connection("B-not-granted-yet", false)
	alias := func(c Connection) string { return "mcp_" + digest(c.ID + "\x00" + toolName)[:48] }
	provider := &automaticCatalogFixture{interrupt: true}
	limits := RunLimits{ActiveTime: 20 * time.Second, ModelTimeout: 3 * time.Second, OutputTokens: 256}
	execute := func(expectedID string) {
		t.Helper()
		run, err := store.claim(ctx)
		if err != nil || run.ID != expectedID {
			t.Fatalf("claim wrong run: %+v %v", run, err)
		}
		store.execute(ctx, provider, run, limits)
	}
	original, err := store.Submit(ctx, "initial-run", "整理现有资料", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	execute(original.ID)
	detail, err := store.Detail(original.TaskID, true)
	if err != nil || detail.Run.Status != "interrupted" || detail.ReadCheckpoint != "ready" ||
		!reflect.DeepEqual(provider.offers, [][]string{{alias(first)}}) {
		t.Fatalf("initial checkpoint missing: %+v %v offers=%v", detail.Run, err, provider.offers)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	store.SetConnectionCredentialKey("stable-automatic-catalog-fixture")
	if err := store.Recover(); err != nil {
		t.Fatal(err)
	}
	if err := store.SetToolPermission(ctx, extra.ID, toolName, tool.Version, extra.Revision, true); err != nil {
		t.Fatal(err)
	}
	resumed, err := store.Resume(ctx, original.TaskID, "resume-old-run", detail.Task.Revision)
	if err != nil || resumed.ID != original.ID || resumed.Generation != 1 {
		t.Fatalf("resume wrong run: %+v %v", resumed, err)
	}
	execute(resumed.ID)
	end, err := store.Detail(original.TaskID, true)
	if err != nil || end.Run.Status != "interrupted" || end.Run.ErrorCode != "read_checkpoint_unavailable" ||
		!reflect.DeepEqual(provider.offers, [][]string{{alias(first)}}) {
		t.Fatalf("old run reinterpreted its authorized catalog: %+v %v offers=%v", end.Run, err, provider.offers)
	}
	if end.Run.Usage == nil || end.Run.Usage.ModelRequests != 1 {
		t.Fatalf("stale catalog attempted replay: %+v", end.Run.Usage)
	}

	fresh, err := store.Submit(ctx, "fresh-run", "请按当前工具配置重新整理", "", original.ConversationID, true)
	if err != nil || fresh.ID == original.ID || fresh.TaskID != original.TaskID {
		t.Fatalf("explicit new run unavailable: %+v %v", fresh, err)
	}
	execute(fresh.ID)
	final, err := store.Detail(original.TaskID, true)
	if err != nil || final.Run.Status != "completed" ||
		!reflect.DeepEqual(provider.offers, [][]string{{alias(first)}, {alias(first), alias(extra)}}) {
		t.Fatalf("fresh run did not use current authorized tools: %+v %v offers=%v", final.Run, err, provider.offers)
	}

	// Older checkpoints hashed the provider/protocol but cannot prove the
	// original automatic tool offer. Even with today's catalog unchanged they
	// must ask for a fresh Run, rather than silently treating it as the old one.
	provider.interrupt = true
	legacy, err := store.Submit(ctx, "legacy-run", "还原旧版本保存点", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	execute(legacy.ID)
	legacyView, err := store.Detail(legacy.TaskID, true)
	if err != nil || legacyView.Run.Status != "interrupted" || legacyView.ReadCheckpoint != "ready" {
		t.Fatalf("legacy fixture checkpoint missing: %+v %v", legacyView.Run, err)
	}
	oldBinding := digest(catalogFixtureBinding + "\n" + automaticReadProtocol)
	if _, err := store.db.Exec("UPDATE run_read_checkpoints SET provider_binding=? WHERE run_id=?", oldBinding, legacy.ID); err != nil {
		t.Fatal(err)
	}
	beforeLegacy := len(provider.offers)
	resumed, err = store.Resume(ctx, legacy.TaskID, "resume-legacy-run", legacyView.Task.Revision)
	if err != nil || resumed.ID != legacy.ID {
		t.Fatalf("legacy resumption fixture not queued: %+v %v", resumed, err)
	}
	execute(resumed.ID)
	legacyView, err = store.Detail(legacy.TaskID, true)
	if err != nil || legacyView.Run.ErrorCode != "read_checkpoint_unavailable" || len(provider.offers) != beforeLegacy {
		t.Fatalf("legacy automatic checkpoint reinterpreted without proof: %+v %v offers=%v", legacyView.Run, err, provider.offers)
	}

}
