package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAssistantA2UISurfacesUseFixedComponentsAndProjection(t *testing.T) {
	longTitle := strings.Repeat("任", 300)
	var tasks []map[string]string
	for i := 0; i < 12; i++ {
		tasks = append(tasks, map[string]string{"title": fmt.Sprintf("%d %s", i, longTitle), "status": "active", "goal": "private-goal", "api_key": "secret-fixture"})
	}
	raw, _ := json.Marshal(tasks)
	surface := assistantResultSurface("list_tasks", http.MethodGet, raw)
	if len(surface) != 2 || surface[0].Version != "1.0" || surface[1].Version != "1.0" || surface[0].CreateSurface == nil || surface[1].UpdateComponents == nil {
		t.Fatalf("invalid message envelopes: %+v", surface)
	}
	created, updated := surface[0].CreateSurface, surface[1].UpdateComponents
	if created.CatalogID != assistantA2UICatalog || created.SurfaceID == "" || created.SurfaceID != updated.SurfaceID {
		t.Fatalf("invalid catalog or surface ID: %+v %+v", created, updated)
	}
	components := updated.Components
	if len(components) != 15 || components[0].ID != "root" || components[0].Component != "Column" {
		t.Fatalf("unexpected component count or root: %+v", components)
	}
	for _, component := range components {
		switch component.Component {
		case "Column":
			if component.ID != "root" || len(component.Children) != 13 {
				t.Fatalf("invalid root: %+v", component)
			}
		case "Text":
			if utf8.RuneCountInString(component.Text) > assistantSurfaceText {
				t.Fatalf("text exceeds limit: %q", component.Text)
			}
		case "Button":
			if component.Child != "details_label" || component.Variant != "borderless" || component.Action == nil || component.Action.Event.Name != "toggle_details" {
				t.Fatalf("unexpected action: %+v", component)
			}
		default:
			t.Fatalf("unexpected component: %+v", component)
		}
	}
	encoded, _ := json.Marshal(surface)
	for _, forbidden := range []string{"private-goal", "secret-fixture", "api_key", "context", "userMessage"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("surface contains %q: %s", forbidden, encoded)
		}
	}
	other := assistantResultSurface("list_tasks", http.MethodGet, raw)
	if other[0].CreateSurface.SurfaceID == created.SurfaceID {
		t.Fatal("surface IDs were reused")
	}
}

func TestAssistantA2UIProjectsKnownReadResults(t *testing.T) {
	tests := []struct {
		tool, raw, want, forbidden string
		button                     bool
	}{
		{"get_current_model_routing", `{"roles":[{"role":"conversation_reply","model_id":"vendor/model","status":"ready","api_key":"hidden"}]}`, "vendor/model", "hidden", false},
		{"get_run_model_receipt", `{"receipts":[{"execution_generation":2,"requested_model":"vendor/requested","model_receipt":{"state":"reported","reported_model":"vendor/reported"},"input":"hidden"}]}`, "vendor/reported", "hidden", false},
		{"get_admission_model_receipt", `{"state":"classified","router_model":{"requested_model":"vendor/requested","reported_model":"vendor/reported","state":"reported"},"reason":"hidden"}`, "vendor/reported", "hidden", false},
		{"list_attention", `{"items":[{"kind":"approval","state":"decision_required","object_ref":"asset:one","private":"hidden"}]}`, "asset:one", "hidden", true},
		{"list_discovery_candidates", `[{"serial":"serial-1","address":"192.0.2.2","state":"observed","secret":"hidden"}]`, "serial-1", "hidden", true},
		{"get_discovery_observations", `{"source":"source-1","scope":"scope-1","items":[{"address":"192.0.2.2","secret":"hidden"}]}`, "192.0.2.2", "hidden", false},
		{"get_task", `{"task":{"title":"清理文件","status":"active"},"messages":[{"text":"hidden"}]}`, "清理文件", "hidden", false},
		{"list_tasks", `[{"title":"[链接](https://evil.example/test)","status":"active"}]`, "链接已省略", "evil.example", true},
		{"list_goals", `[{"target":"巡检","status":"active","acceptance_criteria":"hidden"}]`, "巡检", "hidden", false},
		{"list_goals", `[{"target":"巡检","status":"exhausted","acceptance_criteria":"hidden"}]`, "次数已用完", "hidden", false},
		{"list_artifacts", `[{"name":"月报","accepted_at":123,"content":"hidden"}]`, "已验收", "hidden", false},
		{"list_skills", `[{"scope_label":"审阅","version":3,"active":true,"instructions":"hidden"}]`, "版本 3", "hidden", false},
		{"list_mcp_connections", `[{"name":"目录","revision":2,"enabled":true,"status":"ready","endpoint":"hidden"}]`, "已就绪", "hidden", false},
	}
	for _, tc := range tests {
		t.Run(tc.tool, func(t *testing.T) {
			messages := assistantResultSurface(tc.tool, http.MethodGet, []byte(tc.raw))
			encoded, _ := json.Marshal(messages)
			if !strings.Contains(string(encoded), tc.want) || strings.Contains(string(encoded), tc.forbidden) {
				t.Fatalf("incorrect projection: %s", encoded)
			}
			if got := strings.Contains(string(encoded), "toggle_details"); got != tc.button {
				t.Fatalf("button = %v, want %v: %s", got, tc.button, encoded)
			}
		})
	}
	for _, tool := range []string{"list_tasks", "list_attention", "list_discovery_candidates"} {
		raw := `[]`
		if tool == "list_attention" {
			raw = `{"items":[]}`
		}
		encoded, _ := json.Marshal(assistantResultSurface(tool, http.MethodGet, []byte(raw)))
		if strings.Contains(string(encoded), "toggle_details") {
			t.Fatalf("empty %s unexpectedly has detail action: %s", tool, encoded)
		}
	}
}

func TestAssistantA2UIModelRoutingLabelsAndUnknownValues(t *testing.T) {
	raw := `{"roles":[{"role":"general_router","source":"global_default","status":"demo_role_unavailable","model_id":"vendor/model"},{"role":"unknown_role","source":"unknown_source","status":"unknown_status","model_id":"vendor/other"}]}`
	encoded, _ := json.Marshal(assistantResultSurface("get_current_model_routing", http.MethodGet, []byte(raw)))
	for _, want := range []string{"通用路由", "全局默认模型", "演示模式暂不支持此角色", "vendor/model", "未知角色", "来源未知", "状态未知", "vendor/other"} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("missing %q: %s", want, encoded)
		}
	}
	for _, forbidden := range []string{"general_router", "global_default", "demo_role_unavailable", "unknown_role", "unknown_source", "unknown_status"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("unexpected enum %q: %s", forbidden, encoded)
		}
	}
}

func TestAssistantA2UIEnvelopeAndCatalogCompatibility(t *testing.T) {
	a := setupA1(t, filepath.Join(t.TempDir(), "assistant-a2ui.sqlite"))
	defer a.close()
	res, err := a.ts.Client().Do(a.request(http.MethodGet, "/api/v1/assistant/tools", ""))
	if err != nil {
		t.Fatal(err)
	}
	var catalog []assistantToolView
	if err := json.NewDecoder(res.Body).Decode(&catalog); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if len(catalog) == 0 {
		t.Fatal("empty catalog")
	}
	for _, tool := range catalog {
		if tool.Label == "" {
			t.Fatalf("missing display label: %+v", tool)
		}
	}
	code, completed := a.post("/api/v1/assistant/tools", `{"tool":"list_tasks"}`)
	if code != 200 || completed["state"] != "completed" || completed["result"] == nil || completed["surface"] == nil {
		t.Fatalf("read envelope changed: %d %+v", code, completed)
	}
	code, preview := a.post("/api/v1/assistant/tools", `{"tool":"create_task","body":{"text":"private input","api_key":"private-secret"}}`)
	if code != 200 || preview["state"] != "confirmation_required" || preview["input"] == nil || preview["confirmation_id"] == nil || preview["surface"] == nil {
		t.Fatalf("preview envelope changed: %d %+v", code, preview)
	}
	surface, _ := json.Marshal(preview["surface"])
	for _, forbidden := range []string{preview["confirmation_id"].(string), "private input", "private-secret", "toggle_details"} {
		if strings.Contains(string(surface), forbidden) {
			t.Fatalf("preview surface contains %q: %s", forbidden, surface)
		}
	}
	code, form := a.post("/api/v1/assistant/tools", `{"tool":"configure_model_provider","body":{"api_key":"secret"}}`)
	if code != 200 || form["state"] != "form_required" || form["surface"] != nil {
		t.Fatalf("credential form should remain fixed: %d %+v", code, form)
	}
}
