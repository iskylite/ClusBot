package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

func TestProductionRSIUsesPrivateAccountEndToEnd(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	instance, err := work.Open(filepath.Join(root, "instance.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { instance.Close() })
	const origin = "https://ops.example"
	assets := fstest.MapFS{"index.html": {Data: []byte("test")}}
	s := Server{Store: instance, Origin: origin, Hosts: []string{"ops.example"}}
	f := &accountFixture{t: t, store: instance, handler: s.Handler(assets)}
	ctx := t.Context()
	aliceID := f.setup()
	aliceCookie := f.login("admin", "correct horse battery staple")
	created := f.request("POST", "/api/v1/admin/users", map[string]any{
		"username": "bob", "display_name": "Bob", "password": "fixture long password",
	}, aliceCookie)
	if created.Code != http.StatusCreated {
		t.Fatalf("create fixture member: %d %s", created.Code, created.Body)
	}
	var member struct{ User work.Account }
	accountJSON(t, created, &member)
	bobCookie := f.login("bob", "fixture long password")
	private := func(principal string) *work.Store {
		t.Helper()
		s, err := work.OpenAccountBusinessStore(filepath.Join(t.TempDir(), "private.sqlite"), f.store)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		if err := s.BindAccountBusinessStore(ctx, principal); err != nil {
			t.Fatal(err)
		}
		return s
	}
	alice, bob := private(aliceID), private(member.User.PrincipalID)
	const scope = "业务运行方法"
	const goal = "核实当次证据不沿用此前数字"
	var providerCalls atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		var request map[string]any
		if err := json.Unmarshal(body, &request); err != nil || request["tools"] != nil {
			t.Errorf("evaluation must not call tools: %s (%v)", body, err)
		}
		text := string(body)
		answer := "提示说明"
		if strings.Contains(text, "经审阅") || strings.Contains(text, "用户纠正原文") {
			answer = "当次证据，提示说明"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\"}\n\n", answer)
	}))
	defer provider.Close()
	if err := f.store.InitModelSettings(ctx, work.ModelSettings{ModelID: "fixture-rsi", Protocol: model.ProtocolResponses, BaseURL: provider.URL}); err != nil {
		t.Fatal(err)
	}
	server := Server{Store: f.store, Settings: &model.Settings{Store: f.store}, Origin: "https://ops.example", Hosts: []string{"ops.example"}}
	server.PersonalAPI = func(principal string) (http.Handler, error) {
		switch principal {
		case aliceID:
			return server.PersonalHandler(alice), nil
		case member.User.PrincipalID:
			return server.PersonalHandler(bob), nil
		default:
			return nil, work.ErrAccountDenied
		}
	}
	f.handler = server.Handler(assets)
	check := func(w *httptest.ResponseRecorder, want int) {
		t.Helper()
		if w.Code != want {
			t.Fatalf("status=%d want %d: %s", w.Code, want, w.Body)
		}
	}
	check(f.request(http.MethodGet, "/api/v1/skill-proposals", nil, nil), http.StatusUnauthorized)

	source, err := alice.Submit(ctx, "fixture-rsi-source", goal, scope, "", true)
	if err != nil {
		t.Fatal(err)
	}
	d, err := alice.Detail(source.TaskID, true)
	if err != nil || len(d.Messages) != 1 || d.Messages[0].ID == 0 {
		t.Fatalf("seed verified private user message: %+v %v", d, err)
	}
	w := f.request(http.MethodPost, "/api/v1/experience-candidates/from-correction", map[string]any{"message_id": d.Messages[0].ID}, aliceCookie)
	check(w, http.StatusOK)
	var candidate work.ExperienceCandidate
	accountJSON(t, w, &candidate)
	check(f.request(http.MethodPost, "/api/v1/experience-candidates/"+candidate.ID+"/review", map[string]any{"revision": candidate.Revision, "decision": "approved"}, aliceCookie), http.StatusOK)

	train := []work.ExperienceEvaluationCase{
		{ID: "train-1", Goal: goal, Scope: scope, Prompt: "训练：数据集 A 需要说明", Required: []string{"当次证据"}, Forbidden: []string{"前次数字"}},
		{ID: "train-2", Goal: goal, Scope: scope, Prompt: "训练：数据集 B 需要说明", Required: []string{"当次证据"}, Forbidden: []string{"前次数字"}},
	}
	w = f.request(http.MethodPost, "/api/v1/experience-evaluations", map[string]any{"suite": work.ExperienceEvaluationSuite{Cases: train}}, aliceCookie)
	check(w, http.StatusOK)
	var evaluated work.ExperienceEvaluationRun
	accountJSON(t, w, &evaluated)
	request := map[string]any{"scope_label": scope, "goal": goal, "evaluation_run_id": evaluated.ID}
	check(f.request(http.MethodPost, "/api/v1/skill-proposals", request, bobCookie), http.StatusBadRequest)
	w = f.request(http.MethodPost, "/api/v1/skill-proposals", request, aliceCookie)
	check(w, http.StatusOK)
	var proposal work.SkillProposal
	accountJSON(t, w, &proposal)
	path := "/api/v1/skill-proposals/" + proposal.ID
	approve := func(replay string) map[string]any {
		return map[string]any{"version": proposal.Skill.Version, "revision": proposal.Skill.Revision, "sha256": proposal.Skill.Digest, "replay_id": replay}
	}
	check(f.request(http.MethodPost, path+"/approve", approve(""), aliceCookie), http.StatusConflict)
	w = f.request(http.MethodGet, "/api/v1/skill-proposals", nil, bobCookie)
	check(w, http.StatusOK)
	if strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("other account saw proposal: %s", w.Body)
	}

	holdout := []work.ExperienceEvaluationCase{
		{ID: "held-1", Goal: goal, Scope: scope, Prompt: "留出：变化后的任务输入 C", Required: []string{"当次证据"}, Forbidden: []string{"前次数字"}},
		{ID: "held-2", Goal: goal, Scope: scope, Prompt: "留出：变化后的任务输入 D", Required: []string{"提示说明"}, Forbidden: []string{"前次数字"}},
	}
	replayRequest := struct {
		work.SkillProposalVersion
		Suite work.ExperienceEvaluationSuite `json:"suite"`
	}{work.SkillProposalVersion{Version: proposal.Skill.Version, Revision: proposal.Skill.Revision, Digest: proposal.Skill.Digest}, work.ExperienceEvaluationSuite{Cases: holdout}}
	check(f.request(http.MethodPost, path+"/replay", replayRequest, bobCookie), http.StatusNotFound)
	w = f.request(http.MethodPost, path+"/replay", replayRequest, aliceCookie)
	check(w, http.StatusOK)
	var replay work.SkillReplayReport
	accountJSON(t, w, &replay)
	if !replay.Passed {
		t.Fatalf("private replay did not pass: %+v", replay)
	}
	check(f.request(http.MethodPost, path+"/approve", approve(replay.ID), bobCookie), http.StatusNotFound)
	check(f.request(http.MethodPost, path+"/approve", approve(replay.ID), aliceCookie), http.StatusNoContent)

	rollback := work.SkillProposalVersion{Version: proposal.Skill.Version, Revision: proposal.Skill.Revision + 1, Digest: proposal.Skill.Digest}
	check(f.request(http.MethodPost, path+"/rollback", rollback, bobCookie), http.StatusNotFound)
	check(f.request(http.MethodPost, path+"/rollback", rollback, aliceCookie), http.StatusNoContent)
	w = f.request(http.MethodGet, "/api/v1/skill-proposals", nil, aliceCookie)
	check(w, http.StatusOK)
	var owned []work.SkillProposal
	accountJSON(t, w, &owned)
	if len(owned) != 1 || owned[0].Skill.Active || owned[0].Status != "historical" || providerCalls.Load() != 10 {
		t.Fatalf("fixture lifecycle not isolated: %+v; model calls=%d", owned, providerCalls.Load())
	}
	if versions, err := f.store.Skills(ctx); err != nil || len(versions) != 0 {
		t.Fatalf("instance catalog changed from account operations: %+v %v", versions, err)
	}
}
