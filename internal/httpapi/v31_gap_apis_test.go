package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"clusbot/internal/model"
	"clusbot/internal/work"
)

func TestV31AccountProfileRoleGuardMaterialPreviewAndWorkflowLogs(t *testing.T) {
	instanceDB := filepath.Join(t.TempDir(), "instance.db")
	store, err := work.Open(instanceDB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	settings := &model.Settings{Store: store}
	ctx := context.Background()
	if _, err := settings.Initialize(ctx, work.ModelSettings{
		BaseURL:  "https://api.deepseek.com/v1",
		ModelID:  "deepseek-chat",
		Protocol: model.ProtocolResponses,
		KeyRef:   "env:DEEPSEEK_GAP_TEST_KEY",
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEEPSEEK_GAP_TEST_KEY", "dummy")

	var mu sync.Mutex
	stores := map[string]*work.Store{}
	openPersonal := func(owner string) (*work.Store, error) {
		mu.Lock()
		defer mu.Unlock()
		if existing := stores[owner]; existing != nil {
			return existing, nil
		}
		dir := t.TempDir()
		ps, err := work.Open(filepath.Join(dir, "personal.db"))
		if err != nil {
			return nil, err
		}
		if err := ps.OpenUploads(filepath.Join(dir, "uploads")); err != nil {
			_ = ps.Close()
			return nil, err
		}
		stores[owner] = ps
		t.Cleanup(func() { _ = ps.Close() })
		return ps, nil
	}

	var srv Server
	srv = Server{
		Store:      store,
		Settings:   settings,
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
	f.setup()
	adminCookie := f.login("admin", "correct horse battery staple")

	// 1. 禁止创建第二个超级管理员；支持创建时直接设定首次登录强制改密
	w := f.request("POST", "/api/v1/admin/users", map[string]any{
		"username":             "alex",
		"display_name":         "周工 / Alex",
		"password":             "member password 123",
		"is_superadmin":        true,
		"must_change_password": true,
	}, adminCookie)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "single_superadmin_only") {
		t.Fatalf("expected 409 single_superadmin_only, got %d %s", w.Code, w.Body)
	}

	w = f.request("POST", "/api/v1/admin/users", map[string]any{
		"username":             "alex",
		"display_name":         "周工 / Alex",
		"password":             "temp-password-1234",
		"must_change_password": true,
	}, adminCookie)
	if w.Code != http.StatusCreated {
		t.Fatalf("create member: %d %s", w.Code, w.Body)
	}
	var created struct {
		User work.Account `json:"user"`
	}
	accountJSON(t, w, &created)
	if !created.User.MustChangePassword || created.User.IsSuperadmin {
		t.Fatalf("unexpected created user flags: %+v", created.User)
	}

	// 禁止提权为第二个超管
	w = f.request("PATCH", "/api/v1/admin/users/"+created.User.PrincipalID, map[string]any{
		"is_superadmin": true,
	}, adminCookie)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "single_superadmin_only") {
		t.Fatalf("expected 409 single_superadmin_only on promote, got %d %s", w.Code, w.Body)
	}

	// 本人资料修改 PATCH /api/v1/me/profile
	w = f.request("PATCH", "/api/v1/me/profile", map[string]any{
		"display_name": "林工 (Lin)",
		"avatar":       "林",
	}, adminCookie)
	if w.Code != http.StatusOK {
		t.Fatalf("update my profile: %d %s", w.Code, w.Body)
	}
	var updatedMe work.Account
	accountJSON(t, w, &updatedMe)
	if updatedMe.DisplayName != "林工 (Lin)" || updatedMe.Avatar != "林" {
		t.Fatalf("unexpected updated profile: %+v", updatedMe)
	}

	// 2. 材料上传 -> CSV 在线预览 -> 未发送时可删除
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", "pod7-ib-diag.csv")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("node_id,nvlink_bw,ib_crc_err,stage\npod7-node-01,398.4 GB/s,0,正常验收\npod7-node-03,246.1 GB/s,18,自愈隔离\n"))
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "https://ops.example/api/v1/attachments", &body)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Origin", "https://ops.example")
	req.AddCookie(adminCookie)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload material: %d %s", rec.Code, rec.Body)
	}
	var mat work.Material
	if err := json.Unmarshal(rec.Body.Bytes(), &mat); err != nil {
		t.Fatal(err)
	}

	w = f.request(http.MethodGet, "/api/v1/materials/"+mat.ID+"/preview", nil, adminCookie)
	if w.Code != http.StatusOK {
		t.Fatalf("preview material: %d %s", w.Code, w.Body)
	}
	var prev work.MaterialPreview
	accountJSON(t, w, &prev)
	if prev.PreviewKind != "csv" || len(prev.Columns) != 4 || len(prev.Rows) != 2 || prev.Rows[1][0] != "pod7-node-03" {
		t.Fatalf("unexpected csv preview: %+v", prev)
	}

	downloadURL := "/api/v1/materials/" + mat.ID + "/download"
	w = f.request(http.MethodGet, downloadURL, nil, adminCookie)
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Disposition"), "attachment; filename=pod7-ib-diag.csv") || !strings.Contains(w.Body.String(), "246.1 GB/s,18,自愈隔离") {
		t.Fatalf("download material: %d %v %s", w.Code, w.Header(), w.Body)
	}
	w = f.request(http.MethodGet, downloadURL, nil, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated material download: %d %s", w.Code, w.Body)
	}

	temporaryMember := f.login("alex", "temp-password-1234")
	w = f.request(http.MethodPost, "/api/v1/me/password", map[string]string{
		"current_password": "temp-password-1234",
		"new_password":     "member password changed 123",
	}, temporaryMember)
	if w.Code != http.StatusNoContent {
		t.Fatalf("change member password: %d %s", w.Code, w.Body)
	}
	w = f.request(http.MethodGet, downloadURL, nil, f.login("alex", "member password changed 123"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("download another user's material: %d %s", w.Code, w.Body)
	}

	w = f.request(http.MethodDelete, "/api/v1/materials/"+mat.ID, nil, adminCookie)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete pending material: %d %s", w.Code, w.Body)
	}

	// 3. Token 7 日统计聚合接口
	w = f.request(http.MethodGet, "/api/v1/assistant/token-usage-stats?range=7d", nil, adminCookie)
	if w.Code != http.StatusOK {
		t.Fatalf("token-usage-stats: %d %s", w.Code, w.Body)
	}
	var usageStats tokenUsageStatsResponse
	accountJSON(t, w, &usageStats)
	if usageStats.Range != "7d" || usageStats.DailyBuckets == nil || usageStats.ByModel == nil {
		t.Fatalf("unexpected token usage stats: %+v", usageStats)
	}

	// 4. 故障处置五步流程详情 + 点击步骤读取日志流
	w = f.request(http.MethodPost, "/api/v1/incidents", map[string]any{
		"symptoms":     "POD-07 / n03 NVLink 带宽降速至 62%",
		"impact_scope": "POD-07 / node-03 (GPU-3)",
	}, adminCookie)
	if w.Code != http.StatusCreated {
		t.Fatalf("create incident: %d %s", w.Code, w.Body)
	}
	var inc work.Incident
	accountJSON(t, w, &inc)

	w = f.request(http.MethodGet, "/api/v1/incidents/"+inc.IncidentID+"/workflow", nil, adminCookie)
	if w.Code != http.StatusOK {
		t.Fatalf("incident workflow: %d %s", w.Code, w.Body)
	}
	var wf work.IncidentWorkflowView
	accountJSON(t, w, &wf)
	if len(wf.Stages) != 5 || wf.Stages[0].StepKey != "observe" || wf.Stages[2].StepKey != "self_heal" {
		t.Fatalf("unexpected incident workflow stages: %+v", wf)
	}

	w = f.request(http.MethodGet, "/api/v1/incidents/"+inc.IncidentID+"/steps/observe/logs", nil, adminCookie)
	if w.Code != http.StatusOK {
		t.Fatalf("incident step logs: %d %s", w.Code, w.Body)
	}
	var stepLogs work.IncidentStepLogView
	accountJSON(t, w, &stepLogs)
	if stepLogs.IncidentID != inc.IncidentID || stepLogs.Lines == nil || len(stepLogs.Lines) != 0 {
		t.Fatalf("unexpected incident step logs: %+v", stepLogs)
	}
}
