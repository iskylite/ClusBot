package httpapi

// A2+A3 HTTP 验收：从 API 提交资产验收状态查询，拿到任务，查到带证据的结果。
//
// 覆盖验收点：
//   - 实际提交并查到结果（POST 受理 → 执行 → GET 进度/结果）；
//   - 重复提交幂等（同请求键返回同一任务）；
//   - 无权查询（未认证被拒 401）；
//   - 进度与结果追溯到同一任务。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

// setupA2HTTP 构造带资产与领域查询执行器的服务器。
func setupA2HTTP(t *testing.T, dbPath string) (*a2HTTPClient, *work.Store) {
	t.Helper()
	store, err := work.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// 登记一个资产与验收覆盖（只读查询目标）。
	if err := store.SeedAssetForTest("node-1", "待交付"); err != nil {
		t.Fatal(err)
	}
	settings := &model.Settings{Store: store}
	server := Server{Store: store, Hosts: []string{"127.0.0.1"}, Settings: settings}
	assignFixtureGrants(t, dbPath, businessFixturePrincipal(t, store))
	ts := httptest.NewServer(productionFixtureHandler(t, server, "http://127.0.0.1", fstest.MapFS{"index.html": {Data: []byte("a2")}}))
	// 启动领域查询执行器（消费确定性 work_unit）。
	ctx, cancel := context.WithCancel(context.Background())
	executor, err := store.EnableAssetAcceptanceQuery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	go executor.Run(ctx, 10*time.Millisecond)
	c := &a2HTTPClient{ts: ts, cancel: cancel}
	c.cookie = c.login()
	return c, store
}

type a2HTTPClient struct {
	ts     *httptest.Server
	cookie string
	cancel context.CancelFunc
}

func (c *a2HTTPClient) close() { c.cancel(); c.ts.Close() }

func (c *a2HTTPClient) request(method, path, body string, authed bool) *http.Request {
	req, _ := http.NewRequest(method, c.ts.URL+path, strings.NewReader(body))
	req.Host = "127.0.0.1"
	req.Header.Set("Origin", "http://127.0.0.1")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if authed && c.cookie != "" {
		req.Header.Set("Cookie", c.cookie)
	}
	return req
}

func (c *a2HTTPClient) login() string {
	req, _ := http.NewRequest("POST", c.ts.URL+"/api/v1/session", strings.NewReader(businessFixtureCredentials))
	req.Host = "127.0.0.1"
	req.Header.Set("Origin", "http://127.0.0.1")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.ts.Client().Do(req)
	if err != nil || resp.StatusCode != 200 {
		return ""
	}
	defer resp.Body.Close()
	return resp.Cookies()[0].String()
}

func (c *a2HTTPClient) do(req *http.Request) (int, map[string]any) {
	resp, err := c.ts.Client().Do(req)
	if err != nil {
		return 0, map[string]any{"error": err.Error()}
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestA2HTTPAssetAcceptanceClosedLoop(t *testing.T) {
	c, _ := setupA2HTTP(t, filepath.Join(t.TempDir(), "a2-http.db"))
	defer c.close()

	t.Run("submit_and_query_result", func(t *testing.T) {
		// 提交明确只读查询（object_ref 直接指定）。
		code, out := c.do(c.request("POST", "/api/v1/domain/asset-acceptance",
			`{"client_request_id":"a2-http-1","object_ref":"node-1"}`, true))
		if code != 202 {
			t.Fatalf("submit: code=%d out=%v", code, out)
		}
		taskID, _ := out["task_id"].(string)
		if taskID == "" {
			t.Fatalf("no task_id: %v", out)
		}
		// 轮询进度直到完成（执行器在后台消费）。
		var progress map[string]any
		done := false
		for i := 0; i < 100; i++ {
			gcode, p := c.do(c.request("GET", "/api/v1/domain/asset-acceptance/"+taskID, "", true))
			if gcode != 200 {
				t.Fatalf("progress: %d", gcode)
			}
			progress = p
			if d, _ := p["done"].(bool); d {
				done = true
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !done {
			t.Fatalf("task not done after polling: %v", progress)
		}
		// 结果带证据：验收状态快照追溯到同一任务。
		result, _ := progress["result"].(map[string]any)
		if result == nil {
			t.Fatalf("no persisted result: %v", progress)
		}
		if result["object_ref"] != "node-1" || result["found"] != true {
			t.Errorf("result: %v", result)
		}
		if result["state"] != "待交付" {
			t.Errorf("asset state=%v, want 待交付", result["state"])
		}
		coverage, _ := result["coverage"].(map[string]any)
		if coverage["delivery-ready"] != "covered" {
			t.Errorf("coverage=%v, want delivery-ready covered", coverage)
		}
	})

	t.Run("desktop_chat_without_model", func(t *testing.T) {
		code, submitted := c.do(c.request("POST", "/api/v1/conversations",
			`{"client_message_id":"desktop-query","text":"查询 node-1 的验收状态"}`, true))
		conversation, _ := submitted["conversation_id"].(string)
		if code != 202 || conversation == "" || submitted["model_pending"] == true {
			t.Fatalf("chat submit: %d %v", code, submitted)
		}
		code, detail := c.do(c.request("GET", "/api/v1/conversations/"+conversation, "", true))
		messages, _ := detail["messages"].([]any)
		if code != 200 || len(messages) != 1 {
			t.Fatalf("chat detail: %d %v", code, detail)
		}
		message, _ := messages[0].(map[string]any)
		task, _ := message["asset_query_task_id"].(string)
		if task == "" {
			t.Fatalf("chat lacks query result reference: %v", message)
		}
		for range 100 {
			code, progress := c.do(c.request("GET", "/api/v1/domain/asset-acceptance/"+task, "", true))
			if code != 200 {
				t.Fatalf("query: %d %v", code, progress)
			}
			if progress["done"] == true {
				if progress["state"] != "succeeded" || progress["result"] == nil {
					t.Fatalf("query result: %v", progress)
				}
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("chat query did not complete")
	})

	t.Run("text_intent_recognized", func(t *testing.T) {
		// 用自然语言明确意图提交。
		code, out := c.do(c.request("POST", "/api/v1/domain/asset-acceptance",
			`{"client_request_id":"a2-http-2","text":"查询 node-1 的验收状态"}`, true))
		if code != 202 || out["object_ref"] != "node-1" {
			t.Errorf("text intent: code=%d out=%v", code, out)
		}
	})

	t.Run("same_request_key_idempotent", func(t *testing.T) {
		_, first := c.do(c.request("POST", "/api/v1/domain/asset-acceptance",
			`{"client_request_id":"a2-http-3","object_ref":"node-1"}`, true))
		_, second := c.do(c.request("POST", "/api/v1/domain/asset-acceptance",
			`{"client_request_id":"a2-http-3","object_ref":"node-1"}`, true))
		if first["task_id"] != second["task_id"] || second["duplicate"] != true {
			t.Errorf("same key not idempotent: %v vs %v", first, second)
		}
	})

	t.Run("unauthenticated_query_denied", func(t *testing.T) {
		// 无权查询：未认证被拒 401。
		code, _ := c.do(c.request("POST", "/api/v1/domain/asset-acceptance",
			`{"client_request_id":"a2-http-4","object_ref":"node-1"}`, false))
		if code != 401 {
			t.Errorf("unauthenticated submit: %d, want 401", code)
		}
	})

	t.Run("invalid_task_query_404", func(t *testing.T) {
		code, _ := c.do(c.request("GET", "/api/v1/domain/asset-acceptance/"+strings.Repeat("f", 32), "", true))
		if code != 404 {
			t.Errorf("unknown task: %d, want 404", code)
		}
	})
}

func TestA2HTTPDeterministicTaskRejectsGenericChangesWithoutModel(t *testing.T) {
	c, store := setupA2HTTP(t, filepath.Join(t.TempDir(), "typed-query-http.db"))
	defer c.close()
	defer store.Close()
	code, out := c.do(c.request("POST", "/api/v1/domain/asset-acceptance",
		`{"client_request_id":"typed-query-http","object_ref":"node-1"}`, true))
	taskID, _ := out["task_id"].(string)
	if code != 202 || taskID == "" {
		t.Fatalf("domain submit: code=%d out=%v", code, out)
	}
	d, err := store.Detail(taskID, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []struct{ URL, Body string }{
		{"/api/v1/conversations/" + d.Conversation.ID + "/task", `{"client_request_id":"typed-followup-http","text":"继续办理"}`},
		{"/api/v1/tasks/" + taskID + "/resume", `{"client_request_id":"typed-resume-http","revision":1,"reassign":true}`},
	} {
		code, out := c.do(c.request("POST", path.URL, path.Body, true))
		if code != 409 || out["code"] != "deterministic_task_recovery_unsupported" {
			t.Fatalf("%s did not fail independently of model with actionable conflict: %d %+v", path.URL, code, out)
		}
	}
}
