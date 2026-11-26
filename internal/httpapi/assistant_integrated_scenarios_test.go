package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

const integratedOwnTitle = "窄屏会话原任务"
const integratedOtherTitle = "另一个不替换草稿的任务"
const integratedAsset = "synthetic-browser-owned-node"
const integratedDraft = "这是一条未发送的原会话草稿。"
const integratedNoModelPrompt = "请向我解释这一页的条目如何分页。"

func TestAssistantIntegratedReadScenariosBrowser(t *testing.T) {
	if os.Getenv("CLUSBOT_PLAYWRIGHT_MODULE") == "" || os.Getenv("CLUSBOT_CHROMIUM") == "" {
		t.Skip("set CLUSBOT_PLAYWRIGHT_MODULE and CLUSBOT_CHROMIUM to installed browser tools")
	}
	_, source, _, _ := runtime.Caller(0)
	web := filepath.Join(filepath.Dir(source), "..", "..", "web")
	browser := func(t *testing.T, mode string, payload []byte) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 65*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "node", "tests/mcpSetupBrowserHarness.mjs", mode)
		cmd.Dir, cmd.Stdin = web, bytes.NewReader(payload)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("browser %s: %v\n%s", mode, err, output)
		}
		return output
	}
	var files map[string][]byte
	if err := json.Unmarshal(browser(t, "bundle", nil), &files); err != nil || len(files["index.html"]) == 0 {
		t.Fatalf("could not bundle application in memory: %v", err)
	}
	assets := fstest.MapFS{}
	for name, data := range files {
		assets[name] = &fstest.MapFile{Data: data}
	}
	serve := func(t *testing.T, f *discoveryA2UIFixture, wrap func(http.Handler) http.Handler) string {
		t.Helper()
		server := httptest.NewUnstartedServer(nil)
		addr := server.Listener.Addr().String()
		origin := "http://" + addr
		real := f.server
		real.Origin, real.Hosts = origin, []string{addr}
		server.Config.Handler = real.Handler(assets)
		if wrap != nil {
			server.Config.Handler = wrap(server.Config.Handler)
		}
		server.Start()
		t.Cleanup(server.Close)
		return origin
	}
	launch := func(t *testing.T, args map[string]any) {
		t.Helper()
		data, _ := json.Marshal(args)
		out := browser(t, "browse-integrated-read-scenarios", data)
		t.Log(strings.TrimSpace(string(out)))
	}
	t.Run("independent_cards_and_narrow_focus", func(t *testing.T) {
		f, calls := integratedControlledReadModel(t)
		other, _ := produceAssistantArtifact(t, f.store, integratedOtherTitle, "synthetic other task body", "integrated-narrow-other")
		task, _ := produceAssistantArtifact(t, f.store, integratedOwnTitle, "synthetic original task body", "integrated-narrow-own")
		if task.TaskID == other.TaskID || task.ConversationID == other.ConversationID {
			t.Fatal("fixture did not create independent tasks")
		}
		seedIntegratedTasksAndAssets(t, f, task.ConversationID)
		page, err := f.store.MessageHistory(t.Context(), task.ConversationID, 0, 0, 0)
		if err != nil || len(page.Items) < 2 || page.Items[0].Text != integratedOwnTitle {
			t.Fatalf("original source message did not persist: %v", err)
		}
		launch(t, map[string]any{"scenario": "narrow", "origin": serve(t, f, nil), "conversation": task.ConversationID,
			"task": task.TaskID, "title": integratedOwnTitle, "other": integratedOtherTitle, "asset": integratedAsset,
			"message": page.Items[0].ID, "draft": integratedDraft})
		if calls.Load() != 2 {
			t.Fatalf("card independence and drawer read made %d model calls, expected two selected reads only", calls.Load())
		}
	})
	t.Run("offline_expired_cursor_and_revoked_source", func(t *testing.T) {
		f, calls := integratedControlledReadModel(t)
		task, _ := produceAssistantArtifact(t, f.store, integratedOwnTitle, "synthetic source", "integrated-offline-own")
		seedIntegratedTasksAndAssets(t, f, task.ConversationID)
		anchor, err := f.store.ReadConversationEvents(t.Context(), f.actor, task.ConversationID, "")
		if err != nil || anchor.Cursor == "" {
			t.Fatalf("fixture did not establish a real cursor: %+v %v", anchor, err)
		}
		raw, err := base64.RawURLEncoding.DecodeString(anchor.Cursor)
		var location struct {
			Sequence int64 `json:"s"`
		}
		if err != nil || json.Unmarshal(raw, &location) != nil || location.Sequence < 1 {
			t.Fatalf("could not target the real retained anchor: %v", err)
		}
		db, err := sql.Open("sqlite", f.path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		var applied atomic.Bool
		fixture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" || r.Header.Get("X-ClusBot-Test-Anchor") != anchor.Cursor || !applied.CompareAndSwap(false, true) {
				http.NotFound(w, r)
				return
			}
			err := func() error {
				tx, err := db.BeginTx(r.Context(), nil)
				if err != nil {
					return err
				}
				defer tx.Rollback()
				match := func(res sql.Result, err error) error {
					if err != nil {
						return err
					}
					n, err := res.RowsAffected()
					if err != nil || n != 1 {
						return fmt.Errorf("wanted one exact local fixture row: n=%d err=%v", n, err)
					}
					return nil
				}
				if err := match(tx.ExecContext(r.Context(), `DELETE FROM run_events WHERE seq=? AND run_id IN
(SELECT id FROM runs WHERE conversation_id=?)`, location.Sequence, task.ConversationID)); err != nil {
					return err
				}
				if err := match(tx.ExecContext(r.Context(), `UPDATE authorizations SET revoked_at=? WHERE subject=? AND object_ref=? AND action='read' AND revoked_at=0`,
					time.Now().UnixMilli(), f.actor, integratedAsset)); err != nil {
					return err
				}
				return tx.Commit()
			}()
			if err != nil {
				t.Errorf("failed isolated offline fixture mutation: %v", err)
				http.Error(w, "could not mutate synthetic fixture", 500)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})
		origin := serve(t, f, func(app http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/test-fixture/drop-retained-cursor-and-revoke" {
					fixture.ServeHTTP(w, r)
					return
				}
				app.ServeHTTP(w, r)
			})
		})
		launch(t, map[string]any{"scenario": "offline", "origin": origin, "conversation": task.ConversationID,
			"task": task.TaskID, "title": integratedOwnTitle, "asset": integratedAsset, "cursor": anchor.Cursor, "draft": integratedDraft})
		if calls.Load() != 1 || !applied.Load() {
			t.Fatalf("offline/reconnect replayed an action or fixture was not exercised: calls=%d mutation=%t", calls.Load(), applied.Load())
		}
	})
	t.Run("chromium_back_forward_cache_revoked_asset", func(t *testing.T) {
		f, calls := integratedControlledReadModel(t)
		task, _ := produceAssistantArtifact(t, f.store, integratedOwnTitle, "synthetic source", "integrated-cache-own")
		seedIntegratedTasksAndAssets(t, f, task.ConversationID)
		db, err := sql.Open("sqlite", f.path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		proof := rand.Text()
		var revoked, opened atomic.Bool
		var reads atomic.Int32
		release := make(chan struct{})
		prefix := "/api/v1/conversations/" + task.ConversationID + "/asset-card/"
		origin := serve(t, f, func(app http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/test-fixture/bfcache-away" && r.Method == http.MethodGet {
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					w.Header().Set("Content-Security-Policy", "default-src 'none'")
					io.WriteString(w, "<!doctype html><title>Local history test</title><p>Local history test</p>")
					return
				}
				if r.URL.Path == "/test-fixture/bfcache-revoke" || r.URL.Path == "/test-fixture/bfcache-release" {
					if r.Method != http.MethodPost || r.Header.Get("X-ClusBot-Test-Proof") != proof {
						http.NotFound(w, r)
						return
					}
					if r.URL.Path == "/test-fixture/bfcache-release" {
						if !revoked.Load() || reads.Load() == 0 || !opened.CompareAndSwap(false, true) {
							http.Error(w, "no protected re-read to release", http.StatusConflict)
							return
						}
						close(release)
					} else {
						result, err := db.ExecContext(r.Context(), `UPDATE authorizations SET revoked_at=?
WHERE subject=? AND object_ref=? AND action='read' AND revoked_at=0`, time.Now().UnixMilli(), f.actor, integratedAsset)
						var count int64
						if err == nil {
							count, err = result.RowsAffected()
						}
						if err != nil || count != 1 || !revoked.CompareAndSwap(false, true) {
							http.Error(w, "could not revoke one test grant", http.StatusConflict)
							return
						}
					}
					w.WriteHeader(http.StatusNoContent)
					return
				}
				if r.Method == http.MethodGet && revoked.Load() && strings.HasPrefix(r.URL.Path, prefix) {
					reads.Add(1)
					deadline := time.NewTimer(15 * time.Second)
					defer deadline.Stop()
					select {
					case <-release:
					case <-r.Context().Done():
						return
					case <-deadline.C:
						http.Error(w, "timed out waiting for the browser assertion", http.StatusGatewayTimeout)
						return
					}
				}
				app.ServeHTTP(w, r)
			})
		})
		args, err := json.Marshal(map[string]any{"origin": origin, "conversation": task.ConversationID, "asset": integratedAsset, "draft": integratedDraft, "proof": proof})
		if err != nil {
			t.Fatal(err)
		}
		result := browser(t, "browse-asset-bfcache", args)
		var status struct {
			Persisted bool `json:"persisted"`
		}
		if err := json.Unmarshal(result, &status); err != nil {
			t.Fatalf("invalid BFCache outcome: %v; %s", err, result)
		}
		if !status.Persisted {
			t.Skipf("BFCache restore not exercised: %s", result)
		}
		t.Log(strings.TrimSpace(string(result)))
		if calls.Load() != 1 || !revoked.Load() || !opened.Load() || reads.Load() == 0 {
			t.Fatalf("restore did not prove a local recheck without replay: model=%d revoked=%t checked=%d released=%t", calls.Load(), revoked.Load(), reads.Load(), opened.Load())
		}
	})
	t.Run("no_model_keeps_reads_and_does_not_fake_answer", func(t *testing.T) {
		f := newDiscoveryA2UIFixture(t, func(store *work.Store) *model.Settings { return &model.Settings{Store: store} })
		// This saves an actual ordinary task and acceptance through the local
		// fixture, without configuring any provider for the app.
		task, _ := prepareTaskRecordsBrowserFixture(t, f, integratedOwnTitle)
		recordPath := taskRecordsConversationFixture(t, f, task.TaskID)
		conversation := strings.Split(recordPath, "/")[4]
		launch(t, map[string]any{"scenario": "no_model", "origin": serve(t, f, nil), "conversation": conversation,
			"task": task.TaskID, "title": integratedOwnTitle, "prompt": integratedNoModelPrompt})
	})
}

func integratedControlledReadModel(t *testing.T) (*discoveryA2UIFixture, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	const key = "CLUSBOT_TEST_INTEGRATED_READ_MODEL_KEY"
	t.Setenv(key, "isolated-test-only")
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		var req struct{ Input []struct{ Content string } }
		if err != nil || json.Unmarshal(b, &req) != nil || len(req.Input) != 1 || strings.Contains(string(b), integratedAsset) ||
			strings.Contains(string(b), integratedOwnTitle) || strings.Contains(string(b), integratedOtherTitle) {
			t.Error("integrated read sent IDs, titles, or invalid payload to the model")
			http.Error(w, "invalid fixture input", 400)
			return
		}
		var next string
		switch req.Input[0].Content {
		case `{"requested_action":"query_task_progress"}`:
			next = "show_progress"
		case `{"requested_action":"query_asset_ledger"}`:
			next = "show_ledger"
		default:
			t.Error("reconnect or drawer ran an unexpected model action")
			http.Error(w, "invalid action", 400)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\"}\n\n", fmt.Sprintf(`{"next":%q}`, next))
	}))
	t.Cleanup(provider.Close)
	f := newDiscoveryA2UIFixture(t, func(store *work.Store) *model.Settings {
		s := &model.Settings{Store: store}
		if _, err := s.Initialize(t.Context(), work.ModelSettings{BaseURL: provider.URL, ModelID: "controlled-read-only", Protocol: model.ProtocolResponses, KeyRef: "env:" + key}); err != nil {
			t.Fatal(err)
		}
		return s
	})
	return f, &calls
}

func seedIntegratedTasksAndAssets(t *testing.T, f *discoveryA2UIFixture, conversation string) {
	t.Helper()
	ctx := t.Context()
	if err := f.store.ImportDomainAssetsForAccount(ctx, f.actor, []work.DomainAssetEntry{{ObjectRef: integratedAsset, Kind: "node", Identity: "synthetic-only", State: work.AssetStateInService, Read: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.OpenAssistantTaskList(ctx, f.actor, conversation, "integrated-source-task-list", "查看我的任务"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.OpenAssistantAssetList(ctx, f.actor, conversation, "integrated-source-asset-list", "查看我的资产"); err != nil {
		t.Fatal(err)
	}
}
