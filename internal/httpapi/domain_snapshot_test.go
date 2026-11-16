package httpapi

// V3 领域台账只读快照端点验收：
//   - 未登录 GET 返回 401（认证由 Handler 集中处理）；
//   - 登录后返回 200，所有数组字段为 [] 而非 null，counts 与各数组长度一致；
//   - SeedDomainDemo 幂等注入后，快照能读到资产与其余台账。

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

type domainTestClient struct {
	ts     *httptest.Server
	cookie string
}

func setupDomainHTTP(t *testing.T) (*domainTestClient, *work.Store) {
	t.Helper()
	store, err := work.Open(filepath.Join(t.TempDir(), "domain.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	server := Server{Store: store, Token: "domain-token", Hosts: []string{"127.0.0.1"}, Settings: &model.Settings{Store: store}}
	ts := httptest.NewServer(server.Handler(fstest.MapFS{"index.html": {Data: []byte("v3")}}))
	t.Cleanup(ts.Close)
	c := &domainTestClient{ts: ts}
	c.cookie = c.login(t)
	return c, store
}

func (c *domainTestClient) login(t *testing.T) string {
	t.Helper()
	req, _ := http.NewRequest("POST", c.ts.URL+"/api/v1/session", strings.NewReader("{\"token\":\"domain-token\"}"))
	req.Host = "127.0.0.1"
	req.Header.Set("Origin", "http://127.0.0.1")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || len(resp.Cookies()) == 0 {
		t.Fatalf("login: code=%d cookies=%v", resp.StatusCode, resp.Cookies())
	}
	return resp.Cookies()[0].String()
}

func (c *domainTestClient) get(path string, authed bool) (int, map[string]any) {
	req, _ := http.NewRequest("GET", c.ts.URL+path, nil)
	req.Host = "127.0.0.1"
	if authed {
		req.Header.Set("Cookie", c.cookie)
	}
	resp, err := c.ts.Client().Do(req)
	if err != nil {
		return 0, map[string]any{"error": err.Error()}
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

var snapshotArrayFields = []string{
	"assets", "facts", "relations", "components", "asset_transitions",
	"acceptance_coverage", "object_types", "relation_types", "observations",
	"fault_segments", "self_heal_policies", "self_heal_executions",
	"incidents", "hypotheses", "proposals", "tickets", "delivery_records",
	"deployment_plans", "loans", "handovers", "loan_exceptions", "leases",
	"workers", "authorizations", "domain_packages", "retention_policies",
	"cleanup_queue", "capabilities", "capability_providers",
	"capability_connections", "cases", "case_events", "operations",
}

func TestDomainSnapshotUnauthenticated(t *testing.T) {
	c, _ := setupDomainHTTP(t)
	code, out := c.get("/api/v1/domain/snapshot", false)
	if code != 401 {
		t.Fatalf("未登录应返回 401，实际 code=%d out=%v", code, out)
	}
}

func TestDomainSnapshotArraysAndCounts(t *testing.T) {
	c, _ := setupDomainHTTP(t)
	code, out := c.get("/api/v1/domain/snapshot", true)
	if code != 200 {
		t.Fatalf("登录后应返回 200，实际 code=%d out=%v", code, out)
	}
	counts, ok := out["counts"].(map[string]any)
	if !ok {
		t.Fatalf("counts 缺失或类型错误: %v", out["counts"])
	}
	if _, ok := out["snapshot_at"]; !ok {
		t.Fatal("snapshot_at 缺失")
	}
	for _, field := range snapshotArrayFields {
		value, exists := out[field]
		if !exists {
			t.Fatalf("字段 %s 缺失", field)
		}
		arr, ok := value.([]any)
		if !ok {
			t.Fatalf("字段 %s 不是数组（可能序列化成 null）: %T %v", field, value, value)
		}
		if n, ok := counts[field].(float64); !ok || int(n) != len(arr) {
			t.Fatalf("counts[%s]=%v 与数组长度 %d 不一致", field, counts[field], len(arr))
		}
	}
}

func TestDomainSnapshotAfterSeed(t *testing.T) {
	c, store := setupDomainHTTP(t)
	if err := store.SeedDomainDemo(context.Background()); err != nil {
		t.Fatalf("SeedDomainDemo 失败: %v", err)
	}
	// 幂等：重复调用不重复插入。
	if err := store.SeedDomainDemo(context.Background()); err != nil {
		t.Fatalf("重复 SeedDomainDemo 失败: %v", err)
	}
	code, out := c.get("/api/v1/domain/snapshot", true)
	if code != 200 {
		t.Fatalf("code=%d out=%v", code, out)
	}
	assets, _ := out["assets"].([]any)
	if len(assets) == 0 {
		t.Fatal("seed 后 assets 仍为空")
	}
	t.Logf("seed 后快照: assets=%d facts=%d incidents=%d tickets=%d loans=%d segments=%d deliveries=%d cases=%d operations=%d leases=%d",
		len(assets), len(out["facts"].([]any)), len(out["incidents"].([]any)),
		len(out["tickets"].([]any)), len(out["loans"].([]any)),
		len(out["fault_segments"].([]any)), len(out["delivery_records"].([]any)),
		len(out["cases"].([]any)), len(out["operations"].([]any)), len(out["leases"].([]any)))
	first, _ := assets[0].(map[string]any)
	if first["object_ref"] == nil || first["object_ref"] == "" {
		t.Fatalf("首条资产缺少 object_ref: %v", first)
	}
	tasks, err := store.ListTasks(context.Background(), work.TaskFilter{})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) == 0 {
		t.Fatal("seed 后 tasks 仍为空")
	}
	t.Logf("seed 后任务: tasks=%d 首条=%s/%s", len(tasks), tasks[0].ID, tasks[0].Status)
}
