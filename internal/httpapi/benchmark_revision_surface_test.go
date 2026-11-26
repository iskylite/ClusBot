package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

const revisionFixtureInstruction = "把未派发的集合通信测试移除，先预览影响。"
const revisionParameterInstruction = "把未派发的集合通信时长从60改为90秒。"

func benchmarkRevisionEvent(t *testing.T, reply a2uiDiscoveryReply, name string) map[string]any {
	t.Helper()
	if len(reply.Surface) != 2 {
		t.Fatalf("expected two independently signed options: %+v", reply.Surface)
	}
	for _, item := range reply.Surface {
		surface := item["createSurface"].(map[string]any)
		for _, raw := range surface["components"].([]any) {
			component := raw.(map[string]any)
			if component["id"] == benchmarkRevisionActionButton(name) {
				reply.Surface = []map[string]any{item}
				return renderedA2UIEvent(t, reply, name)
			}
		}
	}
	t.Fatalf("missing benchmark option %s", name)
	return nil
}

func TestBenchmarkRevisionConversationA2UIUserClickAndScopedReplay(t *testing.T) {
	var calls atomic.Int32
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), revisionFixtureInstruction) || !strings.Contains(string(b), "preview_benchmark_revision") || strings.Contains(string(b), "private-node-42") || strings.Contains(string(b), "192.0.2.184") {
			t.Error("revision model received wrong current request or private asset fields")
		}
		answer := fmt.Sprintf(`{"action":"remove_case","case_key":"collective","quote":%q}`, revisionFixtureInstruction)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\"}\n\n", answer)
	}))
	defer modelServer.Close()
	f, provider, task, conversation, sourceRun := newBenchmarkRevisionSurfaceFixture(t, modelServer.URL, revisionFixtureInstruction)
	ctx := t.Context()
	base := "/api/v1/conversations/" + conversation
	path := base + "/a2ui/benchmark-revision/" + sourceRun
	card := f.request("GET", path, "", nil, f.cookie)
	if card.Code != 200 {
		t.Fatalf("source card: %d %s", card.Code, card.Body)
	}
	var response a2uiDiscoveryReply
	accountJSON(t, card, &response)
	event := benchmarkRevisionEvent(t, response, a2uiBenchmarkPreview)
	newSession := f.account.login("admin", "correct horse battery staple")
	if bad := f.request("POST", path, "test-another-session", event, newSession); bad.Code != 409 {
		t.Fatalf("wrong session used card: %d %s", bad.Code, bad.Body)
	}
	wire := event["action"].(map[string]any)["context"].(map[string]any)
	wire["revision"] = 99
	if bad := f.request("POST", path, "test-other-revision", event, f.cookie); bad.Code != 409 {
		t.Fatalf("wrong plan revision: %d", bad.Code)
	}
	wire["revision"] = 1
	wire["text"] = "use different data"
	if bad := f.request("POST", path, "test-extra-field", event, f.cookie); bad.Code != 400 {
		t.Fatal("arbitrary action accepted")
	}
	delete(wire, "text")
	if calls.Load() != 0 {
		t.Fatalf("display or forged actions triggered model: %d", calls.Load())
	}
	// An opt-in headless browser uses a separately built current frontend,
	// the same local fixture and the same user action, not a mock chat view.
	wantRevision := 1
	clickKey := "revision-preview-" + sourceRun
	if chrome := os.Getenv("CLUSBOT_TEST_BROWSER"); chrome != "" {
		benchmarkPreviewInBrowser(t, chrome, f, conversation, task, calls.Load, false)
		wantRevision = 2
	}
	var receipt work.BenchmarkRevisionChatReceipt
	for i := 0; i < 2; i++ {
		result := f.request("POST", path, clickKey, event, f.cookie)
		if result.Code != 200 {
			t.Fatalf("user click: %d %s", result.Code, result.Body)
		}
		var next work.BenchmarkRevisionChatReceipt
		accountJSON(t, result, &next)
		if next.SourceRunID != sourceRun || next.TaskID != task || next.Proposal.ExpectedRevision != 1 || next.Proposal.State != "preview_only" || len(next.Proposal.Preview.Changes.Removed) != 1 || next.Proposal.Preview.Changes.Removed[0] != "collective" || i > 0 && next.RunID != receipt.RunID {
			t.Fatalf("wrong receipt: %+v", next)
		}
		receipt = next
	}
	plan, err := f.store.ReadBenchmarkRevision(ctx, f.actor, task, 0)
	if err != nil || plan.Revision != wantRevision || len(plan.Profile.Cases) != 3-wantRevision || calls.Load() != 1 || provider.starts.Load() != 0 {
		t.Fatalf("preview affected execution, plan, or repeated model: %d %d %v", calls.Load(), provider.starts.Load(), err)
	}
	applied := f.request("POST", base+"/benchmark-revision-preview/"+receipt.RunID+"/confirm", "", map[string]any{"action": "apply_benchmark_revision", "expected_revision": 1}, f.cookie)
	if applied.Code != 200 {
		t.Fatalf("trusted user confirmation: %d %s", applied.Code, applied.Body)
	}
	if stale := f.request("POST", path, "test-preview-after-new-version", event, f.cookie); stale.Code != 409 || calls.Load() != 1 {
		t.Fatalf("stale after plan update: %d calls:%d", stale.Code, calls.Load())
	}
}

func newBenchmarkRevisionSurfaceFixture(t *testing.T, modelURL, instruction string) (*discoveryA2UIFixture, *localIntegrationBenchmark, string, string, string) {
	t.Helper()
	f := newDiscoveryA2UIFixture(t, func(store *work.Store) *model.Settings {
		settings := &model.Settings{Store: store}
		if _, err := settings.Initialize(t.Context(), work.ModelSettings{ModelID: "revision", Protocol: model.ProtocolResponses, BaseURL: modelURL, KeyRef: "env:CLUSBOT_NO_REMOTE_FIXTURE"}); err != nil {
			t.Fatal(err)
		}
		return settings
	})
	ctx := t.Context()
	provider := &localIntegrationBenchmark{}
	f.server.BenchmarkProvider = provider
	f.server.PersonalAPI = func(actor string) (http.Handler, error) {
		if actor != f.actor {
			return nil, work.ErrAccountDenied
		}
		return f.server.PersonalHandler(f.store), nil
	}
	f.account.handler = f.server.Handler(fstest.MapFS{})
	grant := work.DomainBenchmarkGrant{Connection: "bench", Revision: 1, Tool: "test", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	timingGrant := grant
	timingGrant.Allowed = map[string][]string{"duration_sec": {"60", "90"}}
	if err := f.store.ImportDomainAssetsForAccount(ctx, f.actor, []work.DomainAssetEntry{
		{ObjectRef: "node-1", Kind: "node", Identity: "private-node-42", Benchmark: []work.DomainBenchmarkGrant{grant}},
		{ObjectRef: "node-2", Kind: "node", Identity: "private-node-43", Benchmark: []work.DomainBenchmarkGrant{timingGrant}},
	}); err != nil {
		t.Fatal(err)
	}
	var cases []work.BenchmarkCase
	for i, key := range []string{"first", "collective"} {
		c := work.BenchmarkCase{Key: key, Targets: []string{fmt.Sprintf("node-%d", i+1)}, Binding: work.BenchmarkBinding{Connection: "bench", Revision: 1, Tool: "test", Validator: "structured-metrics@1"}, RequiredMetrics: map[string]string{"rate": "GB/s"}}
		if key == "collective" {
			c.Parameters = map[string]string{"duration_sec": "60"}
			c.ActualConfig = map[string]string{"duration_sec": "60"}
			c.Threshold = &work.BenchmarkMetricThreshold{Metric: "rate", Minimum: 120}
			c.ComparisonKeys = []string{"duration_sec"}
		}
		cases = append(cases, c)
	}
	if err := f.store.PublishBenchmarkProfile(ctx, f.actor, work.BenchmarkProfile{Key: "suite", Version: 1, Cases: cases}); err != nil {
		t.Fatal(err)
	}
	task, err := f.store.SubmitBenchmarkRun(ctx, f.actor, work.BenchmarkRunRequest{RequestKey: "bench-a2ui-fixture", Profile: "suite", Version: 1}, provider)
	if err != nil {
		t.Fatal(err)
	}
	chat := f.request("POST", "/api/v1/conversations", "", map[string]any{"client_message_id": "benchmark-chat-open", "text": "/benchmark-status " + task}, f.cookie)
	var conversation struct {
		ID string `json:"conversation_id"`
	}
	accountJSON(t, chat, &conversation)
	base := "/api/v1/conversations/" + conversation.ID
	suggest := f.request("POST", base+"/messages", "", map[string]any{"client_message_id": "benchmark-source-suggestion", "text": instruction, "benchmark_revision_target": map[string]any{"task_id": task, "expected_revision": 1}}, f.cookie)
	var saved struct {
		ID string `json:"run_id"`
	}
	accountJSON(t, suggest, &saved)
	if chat.Code != 202 || suggest.Code != 202 || saved.ID == "" {
		t.Fatalf("could not start through conversation: %d/%d %s %s", chat.Code, suggest.Code, chat.Body, suggest.Body)
	}
	return f, provider, task, conversation.ID, saved.ID
}

func TestBenchmarkRevisionConversationA2UISessionRevokedDuringModel(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-r.Context().Done():
			return
		case <-finish:
		}
		answer := fmt.Sprintf(`{"action":"remove_case","case_key":"collective","quote":%q}`, revisionFixtureInstruction)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\"}\n\n", answer)
	}))
	defer modelServer.Close()
	f, provider, task, conversation, sourceRun := newBenchmarkRevisionSurfaceFixture(t, modelServer.URL, revisionFixtureInstruction)
	base := "/api/v1/conversations/" + conversation
	path := base + "/a2ui/benchmark-revision/" + sourceRun
	card := f.request("GET", path, "", nil, f.cookie)
	if card.Code != 200 {
		t.Fatalf("source card: %d %s", card.Code, card.Body)
	}
	var surface a2uiDiscoveryReply
	accountJSON(t, card, &surface)
	action := benchmarkRevisionEvent(t, surface, a2uiBenchmarkPreview)
	result := make(chan *httptest.ResponseRecorder, 1)
	const key = "revoked-model-preview"
	go func() { result <- f.request("POST", path, key, action, f.cookie) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		close(finish)
		t.Fatal("controlled model was not called")
	}
	logout := f.request("DELETE", "/api/v1/session", "", nil, f.cookie)
	close(finish)
	if logout.Code != 204 {
		t.Fatalf("session logout: %d %s", logout.Code, logout.Body)
	}
	select {
	case got := <-result:
		if got.Code != 403 || strings.Contains(got.Body.String(), "asset_impact") {
			t.Fatalf("revoked session received preview: %d %s", got.Code, got.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("model preview did not finish")
	}
	next := f.account.login("admin", "correct horse battery staple")
	state := f.request("GET", base+"/benchmark-revision-requests/"+key, "", nil, next)
	if state.Code != 200 || !strings.Contains(state.Body.String(), `"state":"interrupted"`) || strings.Contains(state.Body.String(), "preview_run_id") {
		t.Fatalf("revoked session stored a confirmed preview: %d %s", state.Code, state.Body)
	}
	revision, err := f.store.ReadBenchmarkRevision(t.Context(), f.actor, task, 0)
	if err != nil || revision.Revision != 1 || provider.starts.Load() != 0 || calls.Load() != 1 {
		t.Fatalf("auth loss changed the plan, called model again or dispatched: %+v %v", revision, err)
	}
}

func TestBenchmarkRevisionConversationA2UIParameterConsent(t *testing.T) {
	t.Run("default model", func(t *testing.T) { checkBenchmarkRevisionParameterConsent(t, false) })
	t.Run("dedicated revision model", func(t *testing.T) { checkBenchmarkRevisionParameterConsent(t, true) })
}

func checkBenchmarkRevisionParameterConsent(t *testing.T, dedicated bool) {
	t.Helper()
	var plain, selected atomic.Int32
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string
			Input []struct{ Content string }
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 32<<10)).Decode(&req); err != nil || len(req.Input) != 1 {
			t.Errorf("invalid model request: %v", err)
			http.Error(w, "invalid", 400)
			return
		}
		raw := req.Input[0].Content
		if dedicated && (req.Model != "checked-duration-role" || strings.Contains(raw, "expected_actual")) ||
			!dedicated && req.Model != "revision" {
			t.Errorf("wrong model route or disclosed a restricted field: %q", req.Model)
		}
		var input work.BenchmarkRevisionModelInput
		if err := json.Unmarshal([]byte(raw), &input); err != nil || (input.Text != revisionParameterInstruction && input.Text != "把未派发的集合通信时长改为100秒。") || strings.Contains(raw, "private-node") {
			t.Errorf("wrong model request or private facts disclosed: %+v %v", input, err)
		}
		for _, c := range input.Cases {
			for _, field := range c.Parameters {
				if field.MayChangeActual {
					t.Error("only the signed choice can authorize the check; the model never can")
				}
			}
		}
		switch input.RequestedAction {
		case a2uiBenchmarkPreview:
			plain.Add(1)
		case a2uiBenchmarkSyncActual:
			selected.Add(1)
		default:
			t.Errorf("wrong trusted action: %q", input.RequestedAction)
		}
		value := "90"
		if strings.Contains(input.Text, "100") {
			value = "100"
		}
		answer := fmt.Sprintf(`{"action":"set_parameter","case_key":"collective","parameter":"duration_sec","value":%q,"change_expected_actual_config":false,"quote":%q}`, value, input.Text)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\"}\n\n", answer)
	}))
	defer modelServer.Close()
	f, provider, task, conversation, source := newBenchmarkRevisionSurfaceFixture(t, modelServer.URL, revisionParameterInstruction)
	if dedicated {
		const env = "CLUSBOT_CHECKED_DURATION_TEST_KEY"
		t.Setenv(env, "fixture-only-key")
		settings := f.server.Settings
		profile, err := settings.SaveProfile(t.Context(), "", model.ProfileInput{Name: "isolated revision", ModelID: "checked-duration-role",
			BaseURL: modelServer.URL, Protocol: model.ProtocolResponses, KeyRef: "env:" + env})
		if err != nil {
			t.Fatal(err)
		}
		if view, err := settings.SaveModelRoutingPolicy(t.Context(), "benchmark", work.BenchmarkRevisionModelRole,
			model.RoutingPolicyInput{ConfirmEgress: true, Candidates: []model.RoutingCandidateInput{{ProfileID: profile.ID, ProfileRevision: profile.Revision}}}); err != nil || len(view.Candidates) != 1 || view.Candidates[0].Status != "locally_configured" {
			t.Fatalf("dedicated local fixture: %+v %v", view, err)
		}
	}
	base := "/api/v1/conversations/" + conversation
	path := base + "/a2ui/benchmark-revision/" + source
	review := func(path string) a2uiDiscoveryReply {
		t.Helper()
		rec := f.request("GET", path, "", nil, f.cookie)
		if rec.Code != 200 {
			t.Fatalf("review: %d %s", rec.Code, rec.Body)
		}
		var out a2uiDiscoveryReply
		accountJSON(t, rec, &out)
		return out
	}
	surface := review(path)
	optIn := benchmarkRevisionEvent(t, surface, a2uiBenchmarkSyncActual)
	if forged := benchmarkRevisionEvent(t, surface, a2uiBenchmarkPreview); forged != nil {
		action := forged["action"].(map[string]any)
		action["name"], action["sourceComponentId"] = a2uiBenchmarkSyncActual, benchmarkRevisionActionButton(a2uiBenchmarkSyncActual)
		if got := f.request("POST", path, "ordinary-surface-not-consent", forged, f.cookie); got.Code != 409 {
			t.Fatalf("ordinary signature accepted as parameter sync: %d %s", got.Code, got.Body)
		}
	}
	secondCookie := f.account.login("admin", "correct horse battery staple")
	if got := f.request("POST", path, "foreign-session-consent", optIn, secondCookie); got.Code != 409 {
		t.Fatalf("another session consumed the consent: %d %s", got.Code, got.Body)
	}
	fields := optIn["action"].(map[string]any)["context"].(map[string]any)
	fields["value"] = "100"
	if got := f.request("POST", path, "extra-parameter", optIn, f.cookie); got.Code != 400 {
		t.Fatalf("arbitrary UI value accepted: %d %s", got.Code, got.Body)
	}
	delete(fields, "value")
	// Even a genuine source run cannot be used to submit a raw permission bit.
	raw := map[string]any{"action": a2uiBenchmarkPreview, "source_run_id": source,
		"client_request_id": "unscope-conversation-consent", "allow_expected_actual_change": true}
	if got := f.request("POST", base+"/benchmark-revision-preview", "", raw, f.cookie); got.Code != 400 {
		t.Fatalf("raw source request bypassed signed choice: %d %s", got.Code, got.Body)
	}
	if plain.Load()+selected.Load() != 0 {
		t.Fatal("forged or displayed option called the model")
	}

	unapproved := f.request("POST", base+"/messages", "", map[string]any{
		"client_message_id": "out-of-grant-duration", "text": "把未派发的集合通信时长改为100秒。",
		"benchmark_revision_target": map[string]any{"task_id": task, "expected_revision": 1},
	}, f.cookie)
	var outside struct {
		Run string `json:"run_id"`
	}
	accountJSON(t, unapproved, &outside)
	outsidePath := base + "/a2ui/benchmark-revision/" + outside.Run
	outsideChoice := benchmarkRevisionEvent(t, review(outsidePath), a2uiBenchmarkSyncActual)
	if got := f.request("POST", outsidePath, "allow-choice-not-arbitrary-value", outsideChoice, f.cookie); got.Code != 403 {
		t.Fatalf("opt-in raised asset parameter grant: %d %s", got.Code, got.Body)
	}
	// Move the authorized source back to the latest message for the real
	// browser entry. This is still the ordinary signed-in chat receiver.
	init := f.request("POST", base+"/messages", "", map[string]any{
		"client_message_id": "allowed-duration-source", "text": revisionParameterInstruction,
		"benchmark_revision_target": map[string]any{"task_id": task, "expected_revision": 1},
	}, f.cookie)
	var allowed struct {
		Run string `json:"run_id"`
	}
	accountJSON(t, init, &allowed)
	allowedPath := base + "/a2ui/benchmark-revision/" + allowed.Run
	allowedSurface := review(allowedPath)
	assertPlan := func(revision int, want string) {
		t.Helper()
		got, err := f.store.ReadBenchmarkRevision(t.Context(), f.actor, task, 0)
		if err != nil || got.Revision != revision || len(got.Profile.Cases) != 2 ||
			got.Profile.Cases[1].Parameters["duration_sec"] != want || got.Profile.Cases[1].ActualConfig["duration_sec"] != want ||
			got.Profile.Cases[1].RequiredMetrics["rate"] != "GB/s" || got.Profile.Cases[1].Threshold == nil ||
			got.Profile.Cases[1].Threshold.Minimum != 120 || got.Profile.Cases[1].Threshold.Metric != "rate" || provider.starts.Load() != 0 {
			t.Fatalf("changed an unapproved revision/metric or dispatched: %+v %v", got, err)
		}
	}
	if chrome := os.Getenv("CLUSBOT_TEST_BROWSER"); chrome != "" {
		benchmarkPreviewInBrowser(t, chrome, f, conversation, task, func() int32 { return plain.Load() + selected.Load() - 1 }, true)
	} else {
		ordinary := benchmarkRevisionEvent(t, allowedSurface, a2uiBenchmarkPreview)
		if got := f.request("POST", allowedPath, "plain-parameter-chooser", ordinary, f.cookie); got.Code != 400 ||
			!strings.Contains(got.Body.String(), "benchmark_revision_actual_consent_required") {
			t.Fatalf("ordinary preview silently rewrote the expected check: %d %s", got.Code, got.Body)
		}
		assertPlan(1, "60")
		selectedEvent := benchmarkRevisionEvent(t, allowedSurface, a2uiBenchmarkSyncActual)
		out := f.request("POST", allowedPath, "signed-parameter-chooser", selectedEvent, f.cookie)
		if out.Code != 200 {
			t.Fatalf("signed preview failed: %d %s", out.Code, out.Body)
		}
		var receipt work.BenchmarkRevisionChatReceipt
		accountJSON(t, out, &receipt)
		p := receipt.Proposal.ParameterChange
		if receipt.RequestedAction != a2uiBenchmarkSyncActual || p == nil || p.Before != "60" || p.After != "90" ||
			p.ExpectedBefore != "60" || p.ExpectedAfter != "90" || receipt.Proposal.Preview.AssetImpact == nil ||
			receipt.Proposal.Preview.AssetImpact.ScopeChanged {
			t.Fatalf("missing exact reviewed values or frozen assets: %+v %+v", receipt, p)
		}
		assertPlan(1, "60")
		if shared := f.request("POST", allowedPath, "signed-parameter-chooser", ordinary, f.cookie); shared.Code != 409 {
			t.Fatalf("different action consumed the same request key: %d %s", shared.Code, shared.Body)
		}
		apply := f.request("POST", base+"/benchmark-revision-preview/"+receipt.RunID+"/confirm", "",
			map[string]any{"action": "apply_benchmark_revision", "expected_revision": 1}, f.cookie)
		if apply.Code != 200 {
			t.Fatalf("trusted apply: %d %s", apply.Code, apply.Body)
		}
	}
	assertPlan(2, "90")
	if plain.Load() != 1 || selected.Load() != 2 {
		t.Fatalf("unexpected model invocations (ordinary / one denied grant and one explicit consent): %d / %d", plain.Load(), selected.Load())
	}
	if stale := f.request("POST", allowedPath, "late-parameter-chooser", benchmarkRevisionEvent(t, allowedSurface, a2uiBenchmarkSyncActual), f.cookie); stale.Code != 409 || selected.Load() != 2 {
		t.Fatalf("stale click reached the model: %d %s", stale.Code, stale.Body)
	}
}

// This test starts its own disposable browser profile, never the user's. All
// business requests hit the ordinary signed-in React chat and HTTP handlers;
// only the small script which clicks the rendered button is test-only.
func benchmarkPreviewInBrowser(t *testing.T, chrome string, f *discoveryA2UIFixture, conversation, task string, calls func() int32, parameter bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 80*time.Second)
	defer cancel()
	dist := t.TempDir()
	webDir, err := filepath.Abs("../../web")
	if err != nil {
		t.Fatal(err)
	}
	bundle := exec.CommandContext(ctx, filepath.Join(webDir, "node_modules/.bin/vite"), "build", "--outDir", dist)
	bundle.Dir = webDir
	if log, err := bundle.CombinedOutput(); err != nil {
		t.Fatalf("build current frontend: %v %s", err, log)
	}
	index, err := os.ReadFile(filepath.Join(dist, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	var token [16]byte
	if _, err = rand.Read(token[:]); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/fixture-%x", token[:])
	chat := "/chat/" + conversation
	type result struct {
		kind  string
		calls int32
	}
	type observation struct {
		result
		inspected chan struct{}
	}
	evidence := make(chan observation, 16)
	var pageHits, assetHits, jsHits, apiHits, applyHits, metaHits atomic.Int32
	ts := httptest.NewUnstartedServer(nil)
	origin := "http://" + ts.Listener.Addr().String()
	business := f.server
	business.Origin = origin
	business.Hosts = []string{ts.Listener.Addr().String()}
	real := business.Handler(os.DirFS(dist))
	driver := `(async()=>{
const until=async(fn)=>{for(let n=0;n<225;n++){const match=fn();if(match)return match;await new Promise(r=>setTimeout(r,120))}throw Error('timed out: '+document.body.innerText.slice(-600))};
const button=(text)=>[...document.querySelectorAll('button')].find(b=>b.textContent.trim()===text&&!b.disabled);
const applyCard=()=>document.querySelector('[aria-label=基准方案修订]');
const report=(name)=>fetch('` + path + `/event',{method:'POST',body:name});
try {
if(` + fmt.Sprint(parameter) + `){
  if(!sessionStorage.getItem('benchmark-parameter-reload')){
    const preview=await until(()=>button('查看方案修订预览'));
    const optIn=button('同步核对值并预演')?.closest('details');
    if(!optIn||optIn.open)throw Error('secondary choice should start closed');
    await report('visible');
    preview.click();
    await until(()=>applyCard().innerText.includes('执行参数与实测核对值须一致'));
    if(button('应用修订'))throw Error('ordinary action granted the actual-check change');
    await report('needs-consent');
    const choice=await until(()=>optIn.open&&button('同步核对值并预演'));
    choice.click();
    await until(()=>button('应用修订'));
    const summary=applyCard().innerText;
    if(!summary.includes('duration_sec：60 → 90')||!summary.includes('同名实测核对值：60 → 90')||
       !summary.includes('范围不变')||!summary.includes('验收指标和阈值不变'))throw Error('does not show the exact parameter/check/scope');
    await report('previewed');
    sessionStorage.setItem('benchmark-parameter-reload','1');
    location.reload();
  }else{
    const apply=await until(()=>button('应用修订'));
    if(!applyCard().innerText.includes('同名实测核对值：60 → 90'))throw Error('reload lost explicit parameter preview');
    await report('reopened');
    apply.click();
    await until(()=>applyCard().innerText.includes('方案修订已应用，提交为修订 #2'));
    await report('applied');
  }
}else if(!sessionStorage.getItem('benchmark-preview-reload')){
  const preview=await until(()=>button('查看方案修订预览'));
  await report('visible');
  preview.click();
  await until(()=>button('应用修订'));
  const card=applyCard();
  if(!card.innerText.includes('collective')||!card.innerText.includes('范围缩小')||
     !card.innerText.includes('不再列入方案：node-2')||!card.innerText.includes('预演时没有本地占用记录'))throw Error('missing independently labeled scope, diff, or holds');
  if(document.activeElement?.textContent!=='1. 涉及资产')throw Error('new preview focus did not move to scope');
  const details=card.querySelector('details');
  details.querySelector('summary').click();
  if(!details.open||!details.innerText.includes('原方案：node-1、node-2')||!details.innerText.includes('修改后：node-1'))
    throw Error('incomplete disclosure of frozen targets');
  button('收起预演').click();
  await until(()=>button('展开本次预演'));
  if(button('应用修订'))throw Error('hidden preview still actionable');
  button('展开本次预演').click();
  await until(()=>button('应用修订'));
  await report('previewed');
  sessionStorage.setItem('benchmark-preview-reload','1');
  location.reload();
}else{
  const apply=await until(()=>button('应用修订'));
  if(!applyCard().innerText.includes('不再列入方案：node-2'))throw Error('historical preview lost asset scope');
  await report('reopened');
  apply.click();
  const firstLookup=await until(()=>button('核对修订结果'));
  if(button('应用修订')||button('按原预演重试'))throw Error('blind retry while first result unknown');
  await report('missing-ack-before');
  firstLookup.click();
  const retry=await until(()=>button('按原预演重试'));
  await report('same-preview-retry');
  retry.click();
  const committedLookup=await until(()=>button('核对修订结果'));
  if(button('按原预演重试'))throw Error('blind retry after commit');
  await report('missing-ack-after');
  committedLookup.click();
  await until(()=>document.body.innerText.includes('方案修订已应用，提交为修订 #2'));
  await report('applied');
}
}catch(err){await report('error: '+err.message)}
})();`
	ts.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case path:
			http.SetCookie(w, &http.Cookie{Name: f.cookie.Name, Value: f.cookie.Value, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, chat, http.StatusSeeOther)
		case chat:
			pageHits.Add(1)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(bytes.Replace(index, []byte("</body>"), []byte("<script src=\""+path+"/click.js\"></script></body>"), 1))
		case path + "/click.js":
			jsHits.Add(1)
			w.Header().Set("Content-Type", "application/javascript")
			io.WriteString(w, driver)
		case path + "/event":
			b, _ := io.ReadAll(io.LimitReader(r.Body, 1024))
			event := observation{result{string(b), calls()}, make(chan struct{})}
			select {
			case evidence <- event:
			case <-r.Context().Done():
				return
			}
			// Let the test read state before allowing the browser's next
			// explicit click. A buffered evidence channel alone races that click.
			select {
			case <-event.inspected:
				w.WriteHeader(http.StatusNoContent)
			case <-r.Context().Done():
			}
		default:
			if strings.HasPrefix(r.URL.Path, "/api/") {
				apiHits.Add(1)
				if strings.Contains(r.URL.Path, "/benchmark-revision-preview/") {
					switch {
					case r.Method == http.MethodGet && r.URL.Query().Get("metadata") == "1":
						metaHits.Add(1)
					case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/confirm"):
						n := applyHits.Add(1)
						if parameter {
							real.ServeHTTP(w, r)
							return
						}
						if n == 2 {
							captured := httptest.NewRecorder()
							real.ServeHTTP(captured, r)
							if captured.Code != 200 {
								t.Errorf("the real confirmation failed: %d %s", captured.Code, captured.Body)
							}
						} else if n != 1 {
							t.Errorf("browser submitted unexpected automatic confirmation %d", n)
						}
						// Lose the acknowledgement before the first handler and
						// after the second handler commits the same confirmation.
						http.Error(w, "fixture unavailable", http.StatusBadGateway)
						return
					}
				}
			} else if strings.HasPrefix(r.URL.Path, "/assets/") {
				assetHits.Add(1)
			}
			real.ServeHTTP(w, r)
		}
	})
	ts.Start()
	defer ts.Close()
	profile := t.TempDir()
	browser := exec.CommandContext(ctx, chrome, "--headless", "--disable-gpu", "--no-first-run", "--no-default-browser-check", "--disable-background-networking", "--disable-component-update", "--disable-sync", "--disable-domain-reliability", "--disable-breakpad", "--disable-features=OptimizationHints", "--no-proxy-server", "--host-resolver-rules=MAP * ~NOTFOUND,EXCLUDE localhost,EXCLUDE 127.0.0.1", "--user-data-dir="+profile, origin+path)
	var browserLog bytes.Buffer
	browser.Stderr = &browserLog
	if err := browser.Start(); err != nil {
		t.Fatalf("start headless browser: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = browser.Wait(); close(done) }()
	defer func() { _ = browser.Process.Kill(); <-done }()
	checkpoints := []result{{"visible", 0}, {"previewed", 1}, {"reopened", 1}, {"missing-ack-before", 1},
		{"same-preview-retry", 1}, {"missing-ack-after", 1}, {"applied", 1}}
	if parameter {
		checkpoints = []result{{"visible", 0}, {"needs-consent", 1}, {"previewed", 2}, {"reopened", 2}, {"applied", 2}}
	}
	for _, want := range checkpoints {
		select {
		case got := <-evidence:
			func() {
				defer close(got.inspected)
				if got.result != want {
					t.Fatalf("browser checkpoint got %+v want %+v", got, want)
				}
				// Check the durable plan, not the UI label. The harness waits
				// on the report response before applying the next action.
				if got.kind != "visible" {
					at := 1
					if got.kind == "missing-ack-after" || got.kind == "applied" {
						at = 2
					}
					plan, err := f.store.ReadBenchmarkRevision(ctx, f.actor, task, 0)
					if err != nil || plan.Revision != at {
						t.Fatalf("%s: persisted plan %d, expected %d: %v", got.kind, plan.Revision, at, err)
					}
					if parameter {
						value := "60"
						if at == 2 {
							value = "90"
						}
						c := plan.Profile.Cases[1]
						if c.Parameters["duration_sec"] != value || c.ActualConfig["duration_sec"] != value || c.RequiredMetrics["rate"] != "GB/s" || c.Threshold == nil || c.Threshold.Metric != "rate" || c.Threshold.Minimum != 120 {
							t.Fatalf("%s: unexpected persisted conditions %+v", got.kind, c)
						}
					}
				}
			}()
		case <-done:
			log := browserLog.String()
			if len(log) > 600 {
				log = log[:600]
			}
			t.Fatalf("browser exited before %s (HTML %d, bundle %d, driver %d, API %d): %s", want.kind, pageHits.Load(), assetHits.Load(), jsHits.Load(), apiHits.Load(), log)
		case <-ctx.Done():
			t.Fatalf("browser did not reach %s: %v (HTML %d, bundle %d, driver %d, API %d)", want.kind, ctx.Err(), pageHits.Load(), assetHits.Load(), jsHits.Load(), apiHits.Load())
		}
	}
	if parameter {
		if applyHits.Load() != 1 || metaHits.Load() != 0 {
			t.Fatalf("parameter scenario expected one deliberate apply and zero reconciliation, got %d/%d", applyHits.Load(), metaHits.Load())
		}
	} else if applyHits.Load() != 2 || metaHits.Load() != 2 {
		t.Fatalf("expected two deliberate submits and two readbacks, got %d/%d", applyHits.Load(), metaHits.Load())
	}
}
