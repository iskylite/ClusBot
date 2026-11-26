package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

func TestAssistantTaskCardBrowserRoundTrip(t *testing.T) { assistantReadCardBrowserFixture(t, "task") }

func TestAssistantTaskRecordsBrowserRoundTrip(t *testing.T) {
	assistantReadCardBrowserFixture(t, "records")
}

func TestAssistantAssetCardBrowserRoundTrip(t *testing.T) {
	assistantReadCardBrowserFixture(t, "asset")
}

func TestAssistantAttentionCardBrowserRoundTrip(t *testing.T) {
	assistantReadCardBrowserFixture(t, "attention")
}

func TestAssistantArtifactCardBrowserRoundTrip(t *testing.T) {
	assistantReadCardBrowserFixture(t, "artifact")
}

func assistantReadCardBrowserFixture(t *testing.T, kind string) {
	t.Helper()
	if os.Getenv("CLUSBOT_PLAYWRIGHT_MODULE") == "" || os.Getenv("CLUSBOT_CHROMIUM") == "" {
		t.Skip("set CLUSBOT_PLAYWRIGHT_MODULE and CLUSBOT_CHROMIUM to installed browser tools")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	_, source, _, _ := runtime.Caller(0)
	web := filepath.Join(filepath.Dir(source), "..", "..", "web")
	run := func(mode string, stdin []byte) []byte {
		t.Helper()
		command := exec.CommandContext(ctx, "node", "tests/mcpSetupBrowserHarness.mjs", mode)
		command.Dir = web
		command.Stdin = bytes.NewReader(stdin)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		out, err := command.Output()
		if err != nil {
			t.Fatalf("browser %s: %v\n%s", mode, err, stderr.String())
		}
		return out
	}
	var data map[string][]byte
	if err := json.Unmarshal(run("bundle", nil), &data); err != nil || len(data["index.html"]) == 0 {
		t.Fatalf("invalid web asset bundle: %v", err)
	}
	assets := fstest.MapFS{}
	for path, contents := range data {
		assets[path] = &fstest.MapFile{Data: contents}
	}
	title, mode, action, decision := "核对本地任务卡夹具", "browse-task-progress", assistantTaskProgressAction, "show_progress"
	if kind == "asset" {
		title, mode, action, decision = "POD-only-in-ledger", "browse-asset-ledger", assistantAssetLedgerAction, "show_ledger"
	} else if kind == "attention" {
		title, mode, action, decision = "only-local-approval", "browse-attention-reason", assistantAttentionReasonAction, "show_reason"
	} else if kind == "artifact" {
		title, mode, action, decision = artifactFixtureTitle, "browse-artifact-preview", assistantArtifactPreviewAction, "show_preview"
	} else if kind == "records" {
		title, mode = "只读回看已验收报告", "browse-task-records"
	}
	const serial = "local-asset-private-identity"
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(action)) || bytes.Contains(body, []byte(title)) || bytes.Contains(body, []byte(serial)) || bytes.Contains(body, []byte("不要解析")) {
			t.Error("model action did not isolate private fields")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\"}\n\n", fmt.Sprintf(`{"next":%q}`, decision))
	}))
	defer provider.Close()
	t.Setenv("CLUSBOT_TASK_CARD_BROWSER_KEY", "isolated-browser-model-key")
	fixture := newDiscoveryA2UIFixture(t, func(store *work.Store) *model.Settings {
		settings := &model.Settings{Store: store}
		if _, err := settings.Initialize(ctx, work.ModelSettings{ModelID: "task-card-browser-fixture", Protocol: model.ProtocolResponses, BaseURL: provider.URL, KeyRef: "env:CLUSBOT_TASK_CARD_BROWSER_KEY"}); err != nil {
			t.Fatal(err)
		}
		return settings
	})
	var target work.Run
	var artifact work.Artifact
	var err error
	var attentionDB *sql.DB
	const approvalID = "browser-attention-source"
	if kind == "asset" {
		err = fixture.store.ImportDomainAssetsForAccount(ctx, fixture.actor, []work.DomainAssetEntry{
			{ObjectRef: title, Kind: "pod", Identity: serial, State: work.AssetStateInService, Read: true},
		})
	} else if kind == "attention" {
		attentionDB, err = sql.Open("sqlite", fixture.path)
		if err == nil {
			defer attentionDB.Close()
			seedAssistantAttentionFixture(t, attentionDB, approvalID, fixture.actor, title)
		}
	} else if kind == "artifact" {
		target, artifact = produceAssistantArtifact(t, fixture.store, title, artifactFixtureText, "browser-test-artifact")
	} else if kind == "records" {
		target, artifact = prepareTaskRecordsBrowserFixture(t, fixture, title)
	} else {
		target, err = fixture.store.Submit(ctx, "browser-task-card-source", title, "", "", true)
	}
	if err != nil {
		t.Fatal(err)
	}
	listener := httptest.NewUnstartedServer(nil)
	host := listener.Listener.Addr().String()
	origin := "http://" + host
	server := fixture.server
	server.Origin, server.Hosts = origin, []string{host}
	listener.Config.Handler = server.Handler(assets)
	listener.Start()
	defer listener.Close()
	arguments, _ := json.Marshal(map[string]string{"origin": origin, "title": title, "task": target.TaskID, "text": artifact.Content})
	run(mode, arguments)
	if kind == "asset" {
		ledger, err := fixture.store.DomainSnapshot(ctx)
		if err != nil || len(ledger.Assets) != 1 || ledger.Assets[0].ObjectRef != title || ledger.Assets[0].Revision != 1 ||
			len(ledger.Operations) != 0 || len(ledger.AssetTransitions) != 0 || calls.Load() != 1 {
			t.Fatalf("unexpected asset change or replay: calls=%d err=%v", calls.Load(), err)
		}
		return
	}
	if kind == "attention" {
		var decision string
		if err := attentionDB.QueryRow("SELECT decision FROM approvals WHERE approval_id=?", approvalID).Scan(&decision); err != nil ||
			decision != "pending" || calls.Load() != 1 {
			t.Fatalf("approval changed or model replayed: %q calls=%d err=%v", decision, calls.Load(), err)
		}
		return
	}
	record, err := fixture.store.Detail(target.TaskID, true)
	if kind == "records" {
		if err != nil || record.Task.Status != "completed" || record.Run.ID != target.ID || record.Acceptance == nil ||
			record.Acceptance.ArtifactID != artifact.ID || record.Publication != nil || calls.Load() != 1 {
			t.Fatalf("task details reread executed or changed original work: model=%d err=%v", calls.Load(), err)
		}
		return
	}
	if kind == "artifact" {
		if err != nil || record.Artifact == nil || record.Artifact.ID != artifact.ID || record.Task.Status != "waiting_user" ||
			record.Acceptance != nil || record.Publication != nil || calls.Load() != 1 {
			t.Fatalf("preview modified artifact state or sent another model request: calls=%d err=%v", calls.Load(), err)
		}
		return
	}
	if err != nil || record.Run.ID != target.ID || record.Run.Status != "queued" || calls.Load() != 1 {
		t.Fatalf("unexpected execution, lost task, or replay: run=%+v calls=%d err=%v", record.Run, calls.Load(), err)
	}
}
