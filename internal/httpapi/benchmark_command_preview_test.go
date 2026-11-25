package httpapi

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

func TestBenchmarkCommandPreviewHTTPMaterialsAndSafety(t *testing.T) {
	ctx := t.Context()
	t.Setenv("CLUSBOT_PREVIEW_TEST_KEY", "fixture-secret")
	var calls atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var request struct {
			Input []struct {
				Content json.RawMessage `json:"content"`
			} `json:"input"`
			Tools []json.RawMessage `json:"tools"`
		}
		if err := json.Unmarshal(raw, &request); err != nil || len(request.Input) != 1 || len(request.Tools) != 0 {
			t.Errorf("unexpected model request: %s %v", raw, err)
			return
		}
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			URL  string `json:"image_url"`
		}
		if err := json.Unmarshal(request.Input[0].Content, &parts); err != nil || len(parts) != 2 || parts[1].Type != "input_image" || !strings.HasPrefix(parts[1].URL, "data:image/png;base64,") {
			t.Errorf("missing image snapshot: %s %v", request.Input[0].Content, err)
			return
		}
		if text := parts[0].Text; !strings.Contains(text, "排除 node-03") || !strings.Contains(text, "node-1") || !strings.Contains(text, "network") ||
			!strings.Contains(text, "不要占用生产集群") || strings.Contains(text, "fixture-secret") || strings.Contains(text, "private-connection") {
			t.Errorf("unexpected model context: %s", text)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"{\\\"checks\\\":[\\\"targets\\\",\\\"health\\\",\\\"scale\\\"]}\"}\n\ndata: {\"type\":\"response.completed\"}\n\n")
	}))
	defer fake.Close()

	dbPath := filepath.Join(t.TempDir(), "preview-http.sqlite")
	store, err := work.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	settings := &model.Settings{Store: store}
	if _, err := settings.Initialize(ctx, work.ModelSettings{ModelID: "test-model", Protocol: model.ProtocolResponses, BaseURL: fake.URL, KeyRef: "env:CLUSBOT_PREVIEW_TEST_KEY"}); err != nil {
		t.Fatal(err)
	}
	configured, err := store.ModelSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	vision := true
	configured.Attributes = &work.ModelAttributes{Vision: &vision}
	if _, err := store.SaveModelSettings(ctx, configured); err != nil {
		t.Fatal(err)
	}
	profile := work.BenchmarkProfile{Key: "plan", Version: 4, Cases: []work.BenchmarkCase{{Key: "network",
		Binding: work.BenchmarkBinding{Connection: "private-connection", Revision: 1, Tool: "test", Validator: "structured-metrics@1"},
		Targets: []string{"node-1"}, RequiredMetrics: map[string]string{"rate": "GB/s"}}}}
	if err := store.PublishBenchmarkProfile(ctx, businessFixturePrincipal(t, store), profile); err != nil {
		t.Fatal(err)
	}
	deniedProfile := profile
	deniedProfile.Key = "denied"
	deniedProfile.Cases = append([]work.BenchmarkCase(nil), profile.Cases...)
	deniedProfile.Cases[0].Targets = []string{"node-unauthorized"}
	if err := store.PublishBenchmarkProfile(ctx, businessFixturePrincipal(t, store), deniedProfile); err != nil {
		t.Fatal(err)
	}
	grant := work.DomainBenchmarkGrant{Connection: "private-connection", Revision: 1, Tool: "test", ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}
	if err := store.ImportDomainAssetsForAccount(ctx, businessFixturePrincipal(t, store), []work.DomainAssetEntry{{ObjectRef: "node-1", Kind: "node", Identity: "fixture", Benchmark: []work.DomainBenchmarkGrant{grant}}}); err != nil {
		t.Fatal(err)
	}

	if err := store.OpenUploads(filepath.Join(t.TempDir(), "uploads")); err != nil {
		t.Fatal(err)
	}
	material, err := store.UploadMaterial(ctx, "requirements.txt", []byte("不要占用生产集群"))
	if err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, img); err != nil {
		t.Fatal(err)
	}
	imageMaterial, err := store.UploadImage(ctx, "node.png", pngBytes.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	server := Server{Store: store, Settings: settings, Hosts: []string{"127.0.0.1"}}
	a := &a1Server{store: store, ts: httptest.NewServer(businessFixtureHandler(t, server))}
	defer a.ts.Close()
	a.cookie = a.login()
	text := "/benchmark plan@v4\n  只跑 7 个健康节点，排除 node-03\n核对结果  "
	input := map[string]any{"client_request_id": "preview-http-1", "text": text, "attachment_ids": []string{material.ID, imageMaterial.ID}}
	payload, _ := json.Marshal(input)
	code, out := a.post("/api/v1/conversations", string(payload))
	if code != http.StatusAccepted || out["status"] != "completed" || out["task_id"] != "" || calls.Load() != 1 {
		t.Fatalf("preview = %d %+v, calls %d", code, out, calls.Load())
	}
	detail, err := store.Detail(out["conversation_id"].(string), false)
	if err != nil || len(detail.Messages) != 2 || detail.Messages[0].Text != text || len(detail.Messages[0].Materials) != 2 ||
		!strings.Contains(detail.Messages[1].Text, "以上要求尚未应用") {
		t.Fatalf("preview not persisted: %+v %v", detail, err)
	}
	code, again := a.post("/api/v1/conversations", string(payload))
	if code != http.StatusAccepted || again["run_id"] != out["run_id"] || calls.Load() != 1 {
		t.Fatalf("retry = %d %+v, calls %d", code, again, calls.Load())
	}
	tasks, err := store.ListTasks(ctx, work.TaskFilter{Limit: 30})
	if err != nil || len(tasks) != 0 {
		t.Fatalf("preview created tasks: %+v %v", tasks, err)
	}

	for _, tc := range []struct {
		name, path, body string
		status           int
	}{
		{"wrong version", "/api/v1/conversations", `{"client_request_id":"preview-wrong-version","text":"/benchmark plan@v3\n运行"}`, http.StatusNotFound},
		{"unauthorized target", "/api/v1/conversations", `{"client_request_id":"preview-denied-target","text":"/benchmark denied@4\n运行"}`, http.StatusForbidden},
		{"unknown selection", "/api/v1/conversations", `{"client_request_id":"preview-invalid","text":"/benchmark plan@x\n运行"}`, http.StatusBadRequest},
		{"task reference", "/api/v1/conversations", `{"client_request_id":"preview-ref","text":"/benchmark plan@4\n运行","task_ref":{"task_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`, http.StatusBadRequest},
		{"task endpoint", "/api/v1/tasks", `{"client_request_id":"preview-task","text":"/benchmark plan@4\n运行"}`, http.StatusBadRequest},
		{"legacy task reference", "/api/v1/conversations", `{"client_request_id":"legacy-ref","text":"/benchmark plan@4","task_ref":{"task_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`, http.StatusBadRequest},
		{"status task reference", "/api/v1/conversations", `{"client_request_id":"status-ref","text":"/benchmark-status aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","task_ref":{"task_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, out := a.post(tc.path, tc.body); code != tc.status {
				t.Fatalf("HTTP = %d, want %d: %+v", code, tc.status, out)
			}
		})
	}
	for _, command := range []string{"/benchmark plan@4", "/benchmark-status aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"} {
		input := map[string]any{"client_request_id": "legacy-attachment-" + command, "text": command, "attachment_ids": []string{material.ID}}
		payload, _ := json.Marshal(input)
		if code, out := a.post("/api/v1/conversations", string(payload)); code != http.StatusBadRequest {
			t.Fatalf("legacy attachment bypass: %d %+v", code, out)
		}
	}
	req := a.request(http.MethodPost, "/api/v1/conversations", `{"client_request_id":"preview-deterministic","text":"/benchmark plan@4\n运行"}`)
	req.Header.Set("X-ClusBot-Reply-Strategy", "deterministic_only")
	resp, err := a.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || calls.Load() != 1 {
		t.Fatalf("deterministic-only = %d, calls %d", resp.StatusCode, calls.Load())
	}

	// Images must not be silently dropped for a text-only model.
	configured, err = store.ModelSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	vision = false
	configured.Attributes = &work.ModelAttributes{Vision: &vision}
	if _, err := store.SaveModelSettings(ctx, configured); err != nil {
		t.Fatal(err)
	}
	imageInput := map[string]any{"client_request_id": "preview-no-vision", "text": "/benchmark plan@4\n按图片规划", "attachment_ids": []string{imageMaterial.ID}}
	imagePayload, _ := json.Marshal(imageInput)
	if code, out := a.post("/api/v1/conversations", string(imagePayload)); code != http.StatusBadGateway || !strings.Contains(out["message"].(string), "当前模型未启用图片输入") || calls.Load() != 1 {
		t.Fatalf("unsupported image = %d %+v, calls %d", code, out, calls.Load())
	}
	fake.Close()
	if code, out := a.post("/api/v1/conversations", `{"client_request_id":"preview-no-model","text":"/benchmark plan@4\n仅用健康节点"}`); code != http.StatusBadGateway || calls.Load() != 1 {
		t.Fatalf("unavailable model = %d %+v, calls %d", code, out, calls.Load())
	}
	if tasks, err := store.ListTasks(ctx, work.TaskFilter{Limit: 30}); err != nil || len(tasks) != 0 {
		t.Fatalf("failed preview created tasks: %+v %v", tasks, err)
	}
}

func TestBenchmarkCommandPreviewHTTPExplicitTaskReference(t *testing.T) {
	ctx := t.Context()
	t.Setenv("CLUSBOT_REFERENCE_TEST_KEY", "fixture-secret")
	dbPath := filepath.Join(t.TempDir(), "reference-http.sqlite")
	store, err := work.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var calls atomic.Int32
	var selectedTask, raceTask, raceConversation string
	var raceRevision int
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct {
			Input []struct {
				Content string `json:"content"`
			} `json:"input"`
			Instructions string            `json:"instructions"`
			Tools        []json.RawMessage `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Input) == 0 || len(req.Tools) != 0 {
			t.Errorf("invalid model request: %+v %v", req, err)
			return
		}
		last := req.Input[len(req.Input)-1].Content
		payload, _, _ := strings.Cut(last, "\n\n材料（JSON 数据）：")
		var input work.BenchmarkCommandPreviewModelInput
		if err := json.Unmarshal([]byte(payload), &input); err != nil || input.Task == nil || input.Task.Benchmark == nil ||
			len(input.Task.Benchmark.Cases) != 1 || len(input.Task.Benchmark.Cases[0].Targets) != 1 ||
			input.Task.Benchmark.Cases[0].Targets[0] != "node-1" || !strings.Contains(req.Instructions, "冻结目标") {
			t.Errorf("wrong selected task context: %s %v", last, err)
			return
		}
		if input.Text == "校验引用并保留附件" {
			if input.Task.ID != selectedTask || !strings.Contains(last, "附件要求") || strings.Contains(last, raceTask) {
				t.Errorf("wrong task or material scope: %s", last)
			}
		} else if input.Text == "模型期间撤销可见性" {
			if input.Task.ID != raceTask {
				t.Errorf("wrong task before revocation: %+v", input.Task)
			}
			archived := true
			if _, err := store.ChangeConversation(ctx, raceConversation, raceRevision, nil, &archived); err != nil {
				t.Errorf("fixture could not archive conversation: %v", err)
			}
		} else {
			t.Errorf("unexpected requirement: %q", input.Text)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"{\\\"checks\\\":[\\\"targets\\\"]}\"}\n\ndata: {\"type\":\"response.completed\"}\n\n")
	}))
	defer fake.Close()
	settings := &model.Settings{Store: store}
	if _, err := settings.Initialize(ctx, work.ModelSettings{ModelID: "test-model", Protocol: model.ProtocolResponses, BaseURL: fake.URL, KeyRef: "env:CLUSBOT_REFERENCE_TEST_KEY"}); err != nil {
		t.Fatal(err)
	}
	profile := work.BenchmarkProfile{Key: "plan", Version: 4, Cases: []work.BenchmarkCase{{Key: "network",
		Binding: work.BenchmarkBinding{Connection: "bench", Revision: 1, Tool: "test", Validator: "structured-metrics@1"},
		Targets: []string{"node-1"}, RequiredMetrics: map[string]string{"rate": "GB/s"}}}}
	if err := store.PublishBenchmarkProfile(ctx, businessFixturePrincipal(t, store), profile); err != nil {
		t.Fatal(err)
	}
	grant := work.DomainBenchmarkGrant{Connection: "bench", Revision: 1, Tool: "test", ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}
	if err := store.ImportDomainAssetsForAccount(ctx, businessFixturePrincipal(t, store), []work.DomainAssetEntry{{ObjectRef: "node-1", Kind: "node", Identity: "fixture", Benchmark: []work.DomainBenchmarkGrant{grant}}}); err != nil {
		t.Fatal(err)
	}

	selectedTask, err = store.SubmitBenchmarkRun(ctx, businessFixturePrincipal(t, store), work.BenchmarkRunRequest{RequestKey: "reference-fixture-one", Profile: "plan", Version: 4}, benchmarkReadProvider{})
	if err != nil {
		t.Fatal(err)
	}
	raceTask, err = store.SubmitBenchmarkRun(ctx, businessFixturePrincipal(t, store), work.BenchmarkRunRequest{RequestKey: "reference-fixture-two", Profile: "plan", Version: 4}, benchmarkReadProvider{})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := store.Detail(selectedTask, true)
	if err != nil {
		t.Fatal(err)
	}
	race, err := store.Detail(raceTask, true)
	if err != nil {
		t.Fatal(err)
	}
	raceConversation, raceRevision = race.Conversation.ID, race.Conversation.Revision
	if err := store.OpenUploads(filepath.Join(t.TempDir(), "uploads")); err != nil {
		t.Fatal(err)
	}
	material, err := store.UploadMaterial(ctx, "requirement.txt", []byte("附件要求"))
	if err != nil {
		t.Fatal(err)
	}
	server := Server{Store: store, Settings: settings, Hosts: []string{"127.0.0.1"}}
	a := &a1Server{store: store, ts: httptest.NewServer(businessFixtureHandler(t, server))}
	defer a.ts.Close()
	a.cookie = a.login()
	endpoint := "/api/v1/conversations/" + selected.Conversation.ID + "/messages"
	input := map[string]any{"client_request_id": "http-selected-reference", "text": "/benchmark plan@v4\n校验引用并保留附件", "attachment_ids": []string{material.ID}, "task_ref": map[string]string{"task_id": selectedTask}}
	payload, _ := json.Marshal(input)
	code, out := a.post(endpoint, string(payload))
	if code != http.StatusAccepted || out["status"] != "completed" || out["task_id"] != "" || calls.Load() != 1 {
		t.Fatalf("selected reference rejected: %d %+v calls=%d", code, out, calls.Load())
	}
	againCode, again := a.post(endpoint, string(payload))
	if againCode != http.StatusAccepted || again["run_id"] != out["run_id"] || calls.Load() != 1 {
		t.Fatalf("retry did not preserve receipt: %d %+v calls=%d", againCode, again, calls.Load())
	}
	page, err := store.MessageHistory(ctx, selected.Conversation.ID, 0, 0, 0)
	if err != nil || len(page.Items) != 2 || page.Items[0].TaskRef == nil || page.Items[0].TaskRef.TaskID != selectedTask || len(page.Items[0].Materials) != 1 {
		t.Fatalf("message reference or material missing: %+v %v", page, err)
	}
	input["client_request_id"] = "http-cross-reference"
	input["task_ref"] = map[string]string{"task_id": raceTask}
	payload, _ = json.Marshal(input)
	if code, out := a.post(endpoint, string(payload)); code != http.StatusNotFound || calls.Load() != 1 {
		t.Fatalf("cross-conversation task reached model: %d %+v calls=%d", code, out, calls.Load())
	}
	input = map[string]any{"client_request_id": "http-revoked-reference", "text": "/benchmark plan@4\n模型期间撤销可见性", "task_ref": map[string]string{"task_id": raceTask}}
	payload, _ = json.Marshal(input)
	if code, out := a.post("/api/v1/conversations/"+raceConversation+"/messages", string(payload)); code != http.StatusNotFound || calls.Load() != 2 {
		t.Fatalf("revoked reference persisted: %d %+v calls=%d", code, out, calls.Load())
	}
	page, err = store.MessageHistory(ctx, raceConversation, 0, 0, 0)
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("revoked reference stored a message: %+v %v", page, err)
	}
	after, err := store.BenchmarkProgress(ctx, businessFixturePrincipal(t, store), selectedTask)
	if err != nil || after.PlanRevision != 1 || len(after.Cases) != 1 || after.Cases[0].Phase != "queued" {
		t.Fatalf("preview changed benchmark task: %+v %v", after, err)
	}
}
