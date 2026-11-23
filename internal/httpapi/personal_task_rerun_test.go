package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"clusbot/internal/work"
)

func TestPersonalRerunReceiptWhenModelUnavailable(t *testing.T) {
	store, err := work.Open(filepath.Join(t.TempDir(), "rerun.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.OpenWorkspace(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	original, err := store.Submit(t.Context(), "personal-original", "整理测试报告", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	workers := store.Start(ctx, rerunReceiptFixtureProvider{}, 1, work.RunLimits{ActiveTime: time.Minute, ModelTimeout: time.Second, OutputTokens: 128})
	var detail work.Detail
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		detail, err = store.Detail(original.TaskID, true)
		if err != nil {
			t.Fatal(err)
		}
		if detail.Task.Status == "waiting_user" && detail.Artifact != nil {
			break
		}
	}
	cancel()
	workers.Wait()
	if detail.Task.Status != "waiting_user" || detail.Artifact == nil {
		t.Fatal("fixture did not produce a result")
	}
	if err := store.Accept(t.Context(), original.TaskID, detail.Artifact.ID, detail.Task.Revision); err != nil {
		t.Fatal(err)
	}
	detail, err = store.Detail(original.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	submitted, err := store.RerunCompletedTask(t.Context(), original.TaskID, "task-rerun:user:anonymous:known-request", detail.Task.Revision)
	if err != nil {
		t.Fatal(err)
	}
	server := Server{Store: store}
	request := func(id string, revision int) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(fmt.Sprintf(`{"client_request_id":%q,"revision":%d}`, id, revision)))
		r.Header.Set("Content-Type", "application/json")
		r.SetPathValue("id", original.TaskID)
		w := httptest.NewRecorder()
		server.personalRerunCompletedTask(w, r)
		return w
	}
	replay := request("known-request", detail.Task.Revision)
	var receipt map[string]string
	if err := json.Unmarshal(replay.Body.Bytes(), &receipt); err != nil || replay.Code != http.StatusAccepted || receipt["task_id"] != original.TaskID || receipt["run_id"] != submitted.ID {
		t.Fatalf("existing request was not recovered: %d %s %v", replay.Code, replay.Body, err)
	}
	if changed := request("known-request", detail.Task.Revision+1); changed.Code != http.StatusConflict {
		t.Fatalf("changed request: %d %s", changed.Code, changed.Body)
	}
	if fresh := request("unknown-request", detail.Task.Revision); fresh.Code != http.StatusServiceUnavailable {
		t.Fatalf("new request without a model: %d %s", fresh.Code, fresh.Body)
	}
}
