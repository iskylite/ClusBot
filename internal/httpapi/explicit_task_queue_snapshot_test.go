package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"clusbot/internal/work"
)

func TestExplicitTaskQueueSnapshotHTTPRequiresSessionAndUsesPrincipalWithoutModel(t *testing.T) {
	f, err := os.CreateTemp("", "clusbot-explicit-queue-*.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	t.Cleanup(func() {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
				t.Errorf("remove test database: %v", err)
			}
		}
	})
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := t.Context()
	s := Server{Store: store, Hosts: []string{"127.0.0.1"}, Token: "test", MaxPendingExplicitTasks: 3}
	handler := s.Handler(fstest.MapFS{})
	expiry := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	get := func(auth bool) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/v1/runtime/queue/explicit-tasks", nil)
		if auth {
			r.AddCookie(&http.Cookie{Name: "clusbot_session", Value: expiry + "." + s.signature(expiry)})
		}
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, r)
		return out
	}
	if w := get(false); w.Code != http.StatusUnauthorized {
		t.Fatalf("queue endpoint auth failure: %d %s", w.Code, w.Body)
	}
	if _, err := store.AdmitExplicitTask(ctx, "private-other-key", "user:other", "secret foreign task", "", work.Delivery{}, nil, false, 0); err != nil {
		t.Fatal(err)
	}
	assert := func(w *httptest.ResponseRecorder, pending float64) {
		t.Helper()
		if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("queue projection failed: %d %v %s", w.Code, w.Header(), w.Body)
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["scope"] != "current_principal" || body["instance_configured_limit"] != float64(3) ||
			body["pending_receipts"] != pending || body["linked_queued"] != float64(0) || body["linked_running"] != float64(0) {
			t.Fatalf("unexpected scoped projection: %v", body)
		}
		if v, ok := body["oldest_queued_age_ms"]; !ok || v != nil {
			t.Fatalf("missing queue age should be explicit null: %v", body)
		}
		if v, ok := body["oldest_receipt_age_ms"]; !ok || (pending == 0) != (v == nil) {
			t.Fatalf("unexpected oldest own receipt age: %v", body)
		}
		if strings.Contains(w.Body.String(), "private-other-key") || strings.Contains(w.Body.String(), path) ||
			strings.Contains(w.Body.String(), "available_slots") {
			t.Fatalf("queue projection leaks identity/path/global availability: %s", w.Body)
		}
	}
	assert(get(true), 0)
	if _, err := store.AdmitExplicitTask(ctx, "own-test-key", "user:session", "own task", "", work.Delivery{}, nil, false, 0); err != nil {
		t.Fatal(err)
	}
	assert(get(true), 1)
}
