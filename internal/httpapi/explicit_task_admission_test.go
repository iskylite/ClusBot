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

func TestExplicitTaskHTTPBackpressureOnlyOnNewTaskEntry(t *testing.T) {
	dbFile, err := os.CreateTemp("", "clusbot-http-explicit-admission-*.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	path := dbFile.Name()
	t.Cleanup(func() {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
				t.Errorf("remove test database: %v", err)
			}
		}
	})
	if err := dbFile.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := Server{Store: store, Hosts: []string{"127.0.0.1"}, Token: "test", MaxPendingExplicitTasks: 1}
	handler := s.Handler(fstest.MapFS{})
	expiry := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	send := func(path, body string) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1"+path, strings.NewReader(body))
		req.Header.Set("Origin", "http://127.0.0.1")
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: "clusbot_session", Value: expiry + "." + s.signature(expiry)})
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err, w.Body.String())
		}
		return w, out
	}
	const first = `{"client_request_id":"explicit-first","text":"first report"}`
	const next = `{"client_request_id":"explicit-second","text":"next report"}`
	if w, out := send("/api/v1/tasks", first); w.Code != http.StatusAccepted || out["receipt_id"] != "explicit-first" {
		t.Fatalf("initial task not durably accepted: %d %v", w.Code, out)
	}
	if w, out := send("/api/v1/tasks", next); w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "2" ||
		out["code"] != "explicit_task_queue_full" || out["request_key"] != "explicit-second" {
		t.Fatalf("expected stable retryable error: %d %v %v", w.Code, w.Header(), out)
	}
	if _, err := store.ReadAdmission(t.Context(), "explicit-second"); err == nil {
		t.Fatal("capacity rejection created a receipt")
	}
	if w, out := send("/api/v1/tasks", first); w.Code != http.StatusAccepted || out["duplicate"] != true {
		t.Fatalf("full queue blocked exact retry: %d %v", w.Code, out)
	}
	if w, _ := send("/api/v1/tasks", `{"client_request_id":"explicit-first","text":"changed"}`); w.Code != http.StatusConflict {
		t.Fatalf("full queue masked idempotency conflict: %d", w.Code)
	}
	if w, _ := send("/api/v1/conversations", `{"client_request_id":"ordinary-chat","text":"show status"}`); w.Code != http.StatusAccepted {
		t.Fatalf("capacity affected ordinary chat: %d %s", w.Code, w.Body)
	}
	conversation, err := store.Submit(t.Context(), "new-conversation", "unrelated chat", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if w, _ := send("/api/v1/conversations/"+conversation.ConversationID+"/task",
		`{"client_request_id":"ordinary-follow-up","text":"continue this work"}`); w.Code != http.StatusAccepted {
		t.Fatalf("capacity affected work submitted from an existing conversation: %d %s", w.Code, w.Body)
	}
	run, err := store.Submit(t.Context(), "explicit-first", "first report", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := store.Detail(run.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	if w, _ := send("/api/v1/tasks/"+run.TaskID+"/cancel", `{"revision":`+strconv.Itoa(detail.Task.Revision)+`}`); w.Code != http.StatusOK {
		t.Fatalf("capacity affected stop control: %d %s", w.Code, w.Body)
	}
	if w, out := send("/api/v1/tasks", next); w.Code != http.StatusAccepted || out["receipt_id"] != "explicit-second" {
		t.Fatalf("the refused key cannot be reused after release: %d %v", w.Code, out)
	}
}
