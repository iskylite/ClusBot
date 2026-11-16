package httpapi

// V3 决策队列端点验收：待确认列表与批准动作走真实 approvals 表。

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

func TestDecisionsApprove(t *testing.T) {
	store, err := work.Open(filepath.Join(t.TempDir(), "decisions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.SeedDomainDemo(context.Background()); err != nil {
		t.Fatal(err)
	}
	server := Server{Store: store, Token: "dec-token", Hosts: []string{"127.0.0.1"}, Settings: &model.Settings{Store: store}}
	ts := httptest.NewServer(server.Handler(fstest.MapFS{"index.html": {Data: []byte("v3")}}))
	t.Cleanup(ts.Close)
	login, _ := http.NewRequest("POST", ts.URL+"/api/v1/session", strings.NewReader("{\"token\":\"dec-token\"}"))
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
	if pr.StatusCode != 200 {
		t.Fatalf("approve code=%d", pr.StatusCode)
	}
	var approved map[string]any
	json.NewDecoder(pr.Body).Decode(&approved)
	if approved["decision"] != "approved" {
		t.Fatalf("decision=%v", approved)
	}
}
