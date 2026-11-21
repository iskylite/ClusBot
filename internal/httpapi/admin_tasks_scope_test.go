package httpapi

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"clusbot/internal/work"
)

func TestAdminTasksScopeFilterAndReadOnlyNotices(t *testing.T) {
	store, err := work.Open(filepath.Join(t.TempDir(), "instance.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	var mu sync.Mutex
	stores := map[string]*work.Store{}
	openPersonal := func(owner string) (*work.Store, error) {
		mu.Lock()
		defer mu.Unlock()
		if existing := stores[owner]; existing != nil {
			return existing, nil
		}
		ps, err := work.Open(filepath.Join(t.TempDir(), strings.ReplaceAll(owner, ":", "_")+".db"))
		if err != nil {
			return nil, err
		}
		stores[owner] = ps
		t.Cleanup(func() { _ = ps.Close() })
		return ps, nil
	}

	var srv Server
	srv = Server{
		Store:      store,
		ServerMode: true,
		Origin:     "https://ops.example",
		Hosts:      []string{"ops.example"},
		Token:      "legacy-token",
		AdminTaskStores: func() map[string]*work.Store {
			mu.Lock()
			defer mu.Unlock()
			out := make(map[string]*work.Store, len(stores))
			for k, v := range stores {
				out[k] = v
			}
			return out
		},
		PersonalAPI: func(owner string) (http.Handler, error) {
			ps, err := openPersonal(owner)
			if err != nil {
				return nil, err
			}
			return srv.PersonalHandler(ps), nil
		},
	}

	f := &accountFixture{
		t:       t,
		store:   store,
		handler: srv.Handler(fstest.MapFS{"index.html": {Data: []byte("test")}}),
	}

	adminID := f.setup()
	adminCookie := f.login("admin", "correct horse battery staple")

	w := f.request("POST", "/api/v1/admin/users", map[string]any{
		"username":     "member1",
		"display_name": "陈工",
		"password":     "member password 123",
	}, adminCookie)
	if w.Code != http.StatusCreated {
		t.Fatalf("create member: %d %s", w.Code, w.Body)
	}
	var created struct {
		User work.Account `json:"user"`
	}
	accountJSON(t, w, &created)
	memberID := created.User.PrincipalID
	memberCookie := f.login("member1", "member password 123")

	// Create 1 task in admin's personal store and 1 task in member1's personal store
	adminPS, err := openPersonal(adminID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adminPS.Submit(t.Context(), "req-admin-task", "超管自己的运维巡检任务", "", "", true); err != nil {
		t.Fatalf("admin create task: %v", err)
	}
	memberPS, err := openPersonal(memberID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := memberPS.Submit(t.Context(), "req-member-task", "陈工的节点带宽排查任务", "", "", true); err != nil {
		t.Fatalf("member create task: %v", err)
	}

	// Non-superadmin cannot query admin task scopes or notices
	w = f.request("GET", "/api/v1/admin/tasks?scope=mine", nil, memberCookie)
	if w.Code != http.StatusForbidden {
		t.Fatalf("member read admin tasks: %d %s", w.Code, w.Body)
	}
	w = f.request("GET", "/api/v1/admin/task-notices", nil, memberCookie)
	if w.Code != http.StatusForbidden {
		t.Fatalf("member read admin task notices: %d %s", w.Code, w.Body)
	}

	// Invalid scope & owner must fail with 400
	w = f.request("GET", "/api/v1/admin/tasks?scope=invalid", nil, adminCookie)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid scope accepted: %d %s", w.Code, w.Body)
	}
	w = f.request("GET", "/api/v1/admin/tasks?owner=bad-owner", nil, adminCookie)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid owner accepted: %d %s", w.Code, w.Body)
	}

	type adminListResp struct {
		Items []adminTask `json:"items"`
	}

	// scope=all returns both tasks
	w = f.request("GET", "/api/v1/admin/tasks?scope=all", nil, adminCookie)
	if w.Code != http.StatusOK {
		t.Fatalf("scope=all: %d %s", w.Code, w.Body)
	}
	var allResp adminListResp
	accountJSON(t, w, &allResp)
	if len(allResp.Items) != 2 {
		t.Fatalf("scope=all count = %d, want 2", len(allResp.Items))
	}

	// scope=mine returns only admin's task
	w = f.request("GET", "/api/v1/admin/tasks?scope=mine", nil, adminCookie)
	if w.Code != http.StatusOK {
		t.Fatalf("scope=mine: %d %s", w.Code, w.Body)
	}
	var mineResp adminListResp
	accountJSON(t, w, &mineResp)
	if len(mineResp.Items) != 1 || mineResp.Items[0].Owner != adminID {
		t.Fatalf("scope=mine got %+v, want only owner %s", mineResp.Items, adminID)
	}

	// scope=others returns only member's task
	w = f.request("GET", "/api/v1/admin/tasks?scope=others", nil, adminCookie)
	if w.Code != http.StatusOK {
		t.Fatalf("scope=others: %d %s", w.Code, w.Body)
	}
	var othersResp adminListResp
	accountJSON(t, w, &othersResp)
	if len(othersResp.Items) != 1 || othersResp.Items[0].Owner != memberID {
		t.Fatalf("scope=others got %+v, want only owner %s", othersResp.Items, memberID)
	}

	// Explicit owner filter
	w = f.request("GET", "/api/v1/admin/tasks?owner="+memberID, nil, adminCookie)
	if w.Code != http.StatusOK {
		t.Fatalf("owner filter: %d %s", w.Code, w.Body)
	}
	var ownerResp adminListResp
	accountJSON(t, w, &ownerResp)
	if len(ownerResp.Items) != 1 || ownerResp.Items[0].Owner != memberID {
		t.Fatalf("owner filter got %+v, want %s", ownerResp.Items, memberID)
	}

	// Read-only cross-account task notices default to other accounts & mark notice_only=true
	w = f.request("GET", "/api/v1/admin/task-notices", nil, adminCookie)
	if w.Code != http.StatusOK {
		t.Fatalf("task-notices: %d %s", w.Code, w.Body)
	}
	var noticeResp struct {
		Items      []adminTaskNotice `json:"items"`
		NoticeOnly bool              `json:"notice_only"`
	}
	accountJSON(t, w, &noticeResp)
	if !noticeResp.NoticeOnly || len(noticeResp.Items) != 1 || noticeResp.Items[0].Owner != memberID || !noticeResp.Items[0].NoticeOnly {
		t.Fatalf("unexpected task-notices response: %+v", noticeResp)
	}

	// Verify A2UI read-only surface for admin_list_task_notices contains no mutating actions
	surface := assistantResultSurface("admin_list_task_notices", http.MethodGet, w.Body.Bytes())
	if len(surface) != 2 {
		t.Fatalf("expected A2UI surface for admin_list_task_notices, got %+v", surface)
	}
	encoded, _ := json.Marshal(surface)
	if strings.Contains(string(encoded), "toggle_details") || !strings.Contains(string(encoded), memberID) {
		t.Fatalf("unexpected notice surface: %s", encoded)
	}
}
