package httpapi

// 决策队列端点验收：演示申请真实可读，但不能冒充有来源的领域审批。

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

func TestDecisionsCannotApproveUnboundDemoEntry(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "decisions.db")
	store, err := work.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	owner := businessFixturePrincipal(t, store)
	if err := store.SeedDomainDemo(context.Background()); err != nil {
		t.Fatal(err)
	}
	server := Server{Store: store, Hosts: []string{"127.0.0.1"}, Settings: &model.Settings{Store: store}}
	assignFixtureGrants(t, dbPath, owner)
	ts := httptest.NewServer(productionFixtureHandler(t, server, "http://127.0.0.1", fstest.MapFS{"index.html": {Data: []byte("v3")}}))
	t.Cleanup(ts.Close)
	login, _ := http.NewRequest("POST", ts.URL+"/api/v1/session", strings.NewReader(businessFixtureCredentials))
	login.Host = "127.0.0.1"
	login.Header.Set("Origin", "http://127.0.0.1")
	login.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(login)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	cookie := resp.Cookies()[0].String()
	get := func(path string) (int, map[string]any) {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.Host = "127.0.0.1"
		req.Header.Set("Cookie", cookie)
		r, err := ts.Client().Do(req)
		if err != nil {
			return 0, map[string]any{"error": err.Error()}
		}
		defer r.Body.Close()
		var out map[string]any
		json.NewDecoder(r.Body).Decode(&out)
		return r.StatusCode, out
	}
	code, out := get("/api/v1/decisions")
	if code != 200 {
		t.Fatalf("decisions code=%d out=%v", code, out)
	}
	approvals, _ := out["approvals"].([]any)
	if len(approvals) == 0 {
		t.Fatal("seed 后没有待确认")
	}
	first, _ := approvals[0].(map[string]any)
	id, _ := first["approval_id"].(string)
	if id == "" {
		t.Fatalf("approval_id 为空: %v", first)
	}
	post, _ := http.NewRequest("POST", ts.URL+"/api/v1/approvals/"+id+"/decision", strings.NewReader("{\"decision\":\"approved\"}"))
	post.Host = "127.0.0.1"
	post.Header.Set("Origin", "http://127.0.0.1")
	post.Header.Set("Cookie", cookie)
	post.Header.Set("Content-Type", "application/json")
	pr, err := ts.Client().Do(post)
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Body.Close()
	var rejected map[string]any
	if err := json.NewDecoder(pr.Body).Decode(&rejected); err != nil {
		t.Fatal(err)
	}
	if pr.StatusCode != http.StatusConflict || rejected["code"] != "approval_source_unverified" {
		t.Fatalf("unbound approval code=%d response=%v", pr.StatusCode, rejected)
	}
	code, out = get("/api/v1/decisions")
	if code != http.StatusOK {
		t.Fatalf("decisions code=%d out=%v", code, out)
	}
	remaining, _ := out["approvals"].([]any)
	for _, item := range remaining {
		row, _ := item.(map[string]any)
		if row["approval_id"] == id && row["decision"] == "pending" {
			return
		}
	}
	t.Fatalf("refusal changed or removed the original pending entry: %v", remaining)
}
