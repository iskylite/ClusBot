package httpapi

import (
	"bytes"
	"context"
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
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

const settingsFixtureModel = "local-settings-fixture-model"
const settingsFixtureKey = "synthetic-isolated-model-fixture-key"
const settingsFixtureBadKey = "synthetic-isolated-wrong-key"
const settingsFixturePrompt = "怎么理解这个本机演示流程？"
const settingsFixtureReply = "这是通过新配置恢复的本机模拟回复。"
const settingsFixtureSkill = "设置验证专用范围"
const settingsFixtureBodyOne = "synthetic-skill-first-private-instructions"
const settingsFixtureBodyTwo = "synthetic-skill-next-private-instructions"

// This uses the real in-memory frontend bundle, account/settings routes,
// admission loop, and personal SQLite stores. The only model is a controlled
// Responses endpoint on loopback; it cannot execute a model or an operation.
func TestAssistantSettingsScenariosBrowser(t *testing.T) {
	if os.Getenv("CLUSBOT_PLAYWRIGHT_MODULE") == "" || os.Getenv("CLUSBOT_CHROMIUM") == "" {
		t.Skip("set CLUSBOT_PLAYWRIGHT_MODULE and CLUSBOT_CHROMIUM to installed browser tools")
	}
	_, source, _, _ := runtime.Caller(0)
	web := filepath.Join(filepath.Dir(source), "..", "..", "web")
	browser := func(t *testing.T, mode string, payload []byte) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 85*time.Second)
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
	for _, scenario := range []string{"empty_model_recovery", "skill_draft_update_disable"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := &settingsModelTraffic{}
			provider := httptest.NewServer(fixture.handler(t))
			t.Cleanup(provider.Close)
			state, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			instance, err := work.Open(filepath.Join(state, "instance.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { instance.Close() })
			settings := &model.Settings{Store: instance}
			const keyEnv = "CLUSBOT_TEST_ASSISTANT_SETTINGS_SCENARIO_ONLY"
			t.Setenv(keyEnv, settingsFixtureKey)
			initial := work.ModelSettings{BaseURL: provider.URL, Protocol: model.ProtocolResponses, KeyRef: "env:" + keyEnv}
			if scenario == "skill_draft_update_disable" {
				initial.ModelID = settingsFixtureModel
			}
			if _, err := settings.Initialize(t.Context(), initial); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			var mu sync.Mutex
			var workers sync.WaitGroup
			stores := map[string]*work.Store{}
			t.Cleanup(func() {
				cancel()
				workers.Wait()
				for _, personal := range stores {
					personal.Close()
				}
			})
			backend := httptest.NewUnstartedServer(nil)
			addr := backend.Listener.Addr().String()
			origin := "http://" + addr
			server := Server{Store: instance, Settings: settings, Origin: origin, Hosts: []string{addr}}
			server.PersonalAPI = func(actor string) (http.Handler, error) {
				mu.Lock()
				defer mu.Unlock()
				personal := stores[actor]
				if personal == nil {
					var err error
					personal, err = work.OpenAccountBusinessStore(filepath.Join(state, fmt.Sprintf("account-%d.db", len(stores))), instance)
					if err != nil {
						return nil, err
					}
					if err := personal.BindAccountBusinessStore(ctx, actor); err != nil {
						personal.Close()
						return nil, err
					}
					stores[actor] = personal
					active := func() bool { return instance.AccountIsActive(ctx, actor) }
					runs := personal.StartWithModelRoles(ctx, settings, 1,
						work.RunLimits{ActiveTime: 20 * time.Second, ModelTimeout: 5 * time.Second, OutputTokens: 512}, active, settings.RoleAvailable)
					workers.Go(runs.Wait)
					workers.Go(func() { personal.RunChatAdmissions(ctx, active, settings.RouteSubmission, settings.RoleAvailable) })
				}
				return server.PersonalHandler(personal), nil
			}
			backend.Config.Handler = server.Handler(assets)
			backend.Start()
			t.Cleanup(backend.Close)
			payload, err := json.Marshal(map[string]string{
				"scenario": scenario, "origin": origin, "provider": provider.URL, "model": settingsFixtureModel,
				"key": settingsFixtureKey, "badKey": settingsFixtureBadKey, "prompt": settingsFixturePrompt,
				"reply": settingsFixtureReply, "skill": settingsFixtureSkill, "bodyOne": settingsFixtureBodyOne, "bodyTwo": settingsFixtureBodyTwo,
			})
			if err != nil {
				t.Fatal(err)
			}
			out := browser(t, "browse-settings-scenarios", payload)
			t.Log(strings.TrimSpace(string(out)))
			got := [6]int32{fixture.rejected.Load(), fixture.listed.Load(), fixture.tested.Load(), fixture.routed.Load(), fixture.replied.Load(), fixture.skill.Load()}
			want := [6]int32{1, 2, 1, 1, 1, 0}
			if scenario == "skill_draft_update_disable" {
				want = [6]int32{0, 0, 0, 1, 1, 1}
			}
			if got != want {
				t.Fatalf("unexpected provider traffic (rejected/list/probe/router/reply/explicit-skill-preview): got %v want %v", got, want)
			}
		})
	}
}

type settingsModelTraffic struct {
	rejected, listed, tested, routed, replied, skill atomic.Int32
}

func (s *settingsModelTraffic) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/models" && r.Header.Get("Authorization") == "Bearer "+settingsFixtureBadKey {
			s.rejected.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+settingsFixtureKey {
			t.Error("unexpected credential reached the isolated model")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/models" {
			s.listed.Add(1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"data":[{"id":%q}]}`, settingsFixtureModel)
			return
		}
		var req struct {
			Model        string `json:"model"`
			Instructions string `json:"instructions"`
			Input        []struct {
				Content string `json:"content"`
			} `json:"input"`
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 128<<10))
		if r.Method != http.MethodPost || r.URL.Path != "/responses" || err != nil || json.Unmarshal(raw, &req) != nil ||
			req.Model != settingsFixtureModel || len(req.Input) == 0 {
			t.Error("unexpected endpoint, model, or payload")
			http.Error(w, "fixture mismatch", 400)
			return
		}
		for _, secret := range []string{settingsFixtureKey, settingsFixtureBadKey, settingsFixtureBodyOne, settingsFixtureBodyTwo, settingsFixtureSkill} {
			if bytes.Contains(raw, []byte(secret)) {
				t.Error("Skill content/name or a synthetic credential crossed the prompt boundary")
				http.Error(w, "fixture prompt boundary", 400)
				return
			}
		}
		answer := ""
		switch {
		case req.Instructions == "Reply with OK." && len(req.Input) == 1 && req.Input[0].Content == "ClusBot connectivity check.":
			s.tested.Add(1)
			answer = "OK"
		case len(req.Input) == 1 && req.Input[0].Content == `{"requested_action":"preview_conversation_skill","scope_kind":"future_conversation"}`:
			s.skill.Add(1)
			answer = `{"next":"show_binding_preview"}`
		case strings.Contains(req.Instructions, "你是 ClusBot Router") && strings.Contains(req.Input[0].Content, settingsFixturePrompt):
			s.routed.Add(1)
			answer = `{"kind":"chat"}`
		default:
			for _, input := range req.Input {
				if strings.Contains(input.Content, settingsFixturePrompt) {
					s.replied.Add(1)
					answer = settingsFixtureReply
					break
				}
			}
		}
		if answer == "" {
			t.Error("an unexpected request reached the isolated model")
			http.Error(w, "unscripted request", 400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n", answer)
	})
}
