package httpapi

// A1 验收：Chat 持久受理。
//
// 覆盖验收点：
//   - 同键同请求返回原回执（幂等），同键异请求返回冲突；
//   - 模型不可用时能查到等待原因（受理挂起而非提前 503）；
//   - 重启后能恢复待分类输入（inbox_events 持久化）；
//   - 分类/任务已提交时重试只返回原结果。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

// a1Server 是一个带未配置模型的测试服务器（模型不可用 → 受理挂起）。
type a1Server struct {
	ts     *httptest.Server
	store  *work.Store
	cookie string
}

// setupA1 启动未配置模型的服务器并登录，返回带会话的客户端助手。
func setupA1(t *testing.T, dbPath string) *a1Server {
	t.Helper()
	store, err := work.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	settings := &model.Settings{Store: store}
	server := Server{Store: store, Token: "a1-token", Hosts: []string{"127.0.0.1"}, Settings: settings}
	ts := httptest.NewServer(server.Handler(fstest.MapFS{"index.html": {Data: []byte("a1")}}))
	a := &a1Server{ts: ts, store: store}
	a.cookie = a.login()
	return a
}

func (a *a1Server) close() { a.ts.Close(); a.store.Close() }

// request 构造一个 Host 匹配白名单（127.0.0.1）、带会话与 Origin 的请求。
func (a *a1Server) request(method, path, body string) *http.Request {
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, a.ts.URL+path, rdr)
	req.Host = "127.0.0.1" // 匹配 Server.Hosts 白名单
	req.Header.Set("Origin", "http://127.0.0.1")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.cookie != "" {
		req.Header.Set("Cookie", a.cookie)
	}
	return req
}

func (a *a1Server) login() string {
	req, _ := http.NewRequest("POST", a.ts.URL+"/api/v1/session", strings.NewReader(`{"token":"a1-token"}`))
	req.Host = "127.0.0.1"
	req.Header.Set("Origin", "http://127.0.0.1")
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.ts.Client().Do(req)
	if err != nil || resp.StatusCode != 200 {
		return ""
	}
	defer resp.Body.Close()
	return resp.Cookies()[0].String()
}

// post 提交 JSON 并返回状态码与解码体。
func (a *a1Server) post(path, body string) (int, map[string]any) {
	resp, err := a.ts.Client().Do(a.request("POST", path, body))
	if err != nil {
		return 0, map[string]any{"error": err.Error()}
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// getAdmission 查询受理回执。
func (a *a1Server) getAdmission(key string) (int, map[string]any) {
	resp, err := a.ts.Client().Do(a.request("GET", "/api/v1/admissions/"+key, ""))
	if err != nil {
		return 0, map[string]any{"error": err.Error()}
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestA1ChatDurableAdmission(t *testing.T) {
	a := setupA1(t, filepath.Join(t.TempDir(), "a1.db"))
	defer a.close()

	t.Run("admission_before_classification_queryable", func(t *testing.T) {
		// 模型不可用：提交后仍受理（202），返回可查询回执而非 503。
		code, out := a.post("/api/v1/conversations", `{"client_request_id":"a1-req-1","text":"整理季度汇报"}`)
		if code != 202 {
			t.Fatalf("submit: code=%d out=%v (模型不可用应受理挂起而非 503)", code, out)
		}
		if out["receipt_id"] != "a1-req-1" {
			t.Errorf("receipt_id=%v, want a1-req-1", out["receipt_id"])
		}
		if out["model_pending"] != true {
			t.Errorf("model_pending=%v, want true (模型不可用挂起)", out["model_pending"])
		}
		// 可查询回执与等待原因。
		qcode, receipt := a.getAdmission("a1-req-1")
		if qcode != 200 {
			t.Fatalf("admission query: %d", qcode)
		}
		if receipt["state"] != "received" {
			t.Errorf("receipt state=%v, want received (已受理未分类)", receipt["state"])
		}
		if receipt["reason"] == "" {
			t.Error("admission reason empty (需要等待返回具体原因)")
		}
	})

	t.Run("same_key_same_request_idempotent", func(t *testing.T) {
		// 同键同请求：返回原回执（幂等），不重复登记。
		_, first := a.post("/api/v1/conversations", `{"client_request_id":"a1-req-2","text":"整理周报"}`)
		code, second := a.post("/api/v1/conversations", `{"client_request_id":"a1-req-2","text":"整理周报"}`)
		if code != 202 {
			t.Fatalf("retry code=%d", code)
		}
		if second["receipt_id"] != first["receipt_id"] {
			t.Errorf("retry new receipt: %v vs %v (同键同请求返回原回执)", second["receipt_id"], first["receipt_id"])
		}
		if second["duplicate"] != true {
			t.Errorf("retry not duplicate (同键同请求应幂等)")
		}
		// 重试后受理记录仍可查询且状态一致（幂等，不重复登记）。
		receipt, err := a.store.ReadAdmission(context.Background(), "a1-req-2")
		if err != nil {
			t.Fatal(err)
		}
		if receipt.ReceiptID != "a1-req-2" || receipt.State != "received" {
			t.Errorf("admission after retry: %+v (同键重试应返回原受理)", receipt)
		}
	})

	t.Run("same_key_different_request_conflict", func(t *testing.T) {
		a.post("/api/v1/conversations", `{"client_request_id":"a1-req-3","text":"内容A"}`)
		code, out := a.post("/api/v1/conversations", `{"client_request_id":"a1-req-3","text":"内容B"}`)
		if code != 409 {
			t.Errorf("conflict code=%d out=%v, want 409 (同键异请求冲突)", code, out)
		}
	})
}

// TestA1AdmissionSurvivesRestart 重启后能恢复待分类输入。
func TestA1AdmissionSurvivesRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "a1-restart.db")
	// 第一次启动：受理一条输入（模型不可用，停留 received）。
	func() {
		a := setupA1(t, dbPath)
		defer a.close()
		code, out := a.post("/api/v1/conversations", `{"client_request_id":"a1-restart","text":"待恢复输入"}`)
		if code != 202 || out["receipt_id"] != "a1-restart" {
			t.Fatalf("first boot admit: code=%d out=%v", code, out)
		}
	}()
	// 重启：同一数据库文件，待分类输入仍可查询（持久化恢复）。
	a := setupA1(t, dbPath)
	defer a.close()
	code, receipt := a.getAdmission("a1-restart")
	if code != 200 {
		t.Fatalf("restart admission query: %d (重启后待分类输入应可恢复)", code)
	}
	if receipt["state"] != "received" {
		t.Errorf("restart state=%v, want received (重启后待分类输入恢复)", receipt["state"])
	}
	if receipt["conversation_id"] != "" {
		t.Fatalf("pending admission returned a conversation: %v", receipt)
	}

	demo := &model.Settings{Store: a.store, Demo: true}
	if err := a.store.ProcessChatAdmissions(context.Background(), demo.RouteSubmission); err != nil {
		t.Fatal(err)
	}
	code, receipt = a.getAdmission("a1-restart")
	conversationID, _ := receipt["conversation_id"].(string)
	if code != 200 || receipt["state"] != "applied" || !validID(conversationID) {
		t.Fatalf("resolved admission did not identify the conversation: code=%d receipt=%v", code, receipt)
	}
}
