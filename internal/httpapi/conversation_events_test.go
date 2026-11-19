package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"clusbot/internal/work"
)

const privateFixtureQuestion = "fixture question must not appear in the event feed"

type waitingInputFixture struct{}

func (waitingInputFixture) Stream(context.Context, []work.Message, bool, int, func(string) error) (*work.TokenUsage, error) {
	return nil, work.ErrTaskAnswer
}

func (waitingInputFixture) StreamTaskAnswer(context.Context, []work.Message, int, func(string) error) (work.TaskAnswer, error) {
	return work.TaskAnswer{Question: privateFixtureQuestion}, nil
}

func TestConversationEventsReconnectReportsInputRequiredWithoutQuestion(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "input-required-reconnect.sqlite")
	a := setupA1(t, path)
	defer func() { a.close() }()
	run, err := a.store.Submit(ctx, "conv-input-recovery", "local task", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "/api/v1/conversations/" + run.ConversationID + "/events"
	fetch := func(path string, want int) work.ConversationEventPage {
		t.Helper()
		res, err := a.ts.Client().Do(a.request(http.MethodGet, path, ""))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		if err != nil || res.StatusCode != want {
			t.Fatalf("feed request returned %d: %s (%v)", res.StatusCode, body, err)
		}
		if strings.Contains(string(body), privateFixtureQuestion) {
			t.Fatal("question text leaked through the feed projection")
		}
		var event work.ConversationEventPage
		if err := json.Unmarshal(body, &event); err != nil {
			t.Fatal(err)
		}
		return event
	}
	before := fetch(endpoint, http.StatusOK)
	if before.Cursor == "" {
		t.Fatal("missing queued cursor")
	}
	workerCtx, stop := context.WithCancel(ctx)
	workers := a.store.Start(workerCtx, waitingInputFixture{}, 1, work.RunLimits{ActiveTime: 5 * time.Second, ModelTimeout: time.Second, OutputTokens: 256})
	defer func() { stop(); workers.Wait() }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		detail, err := a.store.Detail(run.TaskID, true)
		if err != nil {
			t.Fatal(err)
		}
		if detail.Run != nil && detail.Run.Status == "input_required" {
			if detail.Run.Response != privateFixtureQuestion || detail.Task.Status != "waiting_user" {
				t.Fatalf("worker did not persist the real wait: %+v", detail)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker did not enter input_required: %+v", detail)
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	workers.Wait()
	a.close()
	a = setupA1(t, path)
	after := fetch(endpoint+"?cursor="+before.Cursor, http.StatusOK)
	if len(after.Events) == 0 {
		t.Fatal("reconnect lost waiting run")
	}
	last := after.Events[len(after.Events)-1]
	if last.Kind != "run.state_changed" || last.Status != "input_required" || last.RunID != run.ID || last.TaskID != run.TaskID {
		t.Fatalf("wrong safe waiting projection: %+v", last)
	}
	if empty := fetch(endpoint+"?cursor="+after.Cursor, http.StatusOK); len(empty.Events) != 0 || empty.Cursor != after.Cursor {
		t.Fatalf("unchanged reconnect duplicated the waiting marker: %+v", empty)
	}
}

func TestConversationHistoryTaskRefIsExplicitAndReadOnly(t *testing.T) {
	ctx := t.Context()
	a := setupA1(t, filepath.Join(t.TempDir(), "task-ref-api.db"))
	defer a.close()
	r, err := a.store.Submit(ctx, "task-ref-test-origin", "needs input", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, stop := context.WithCancel(ctx)
	workers := a.store.Start(workerCtx, waitingInputFixture{}, 1, work.RunLimits{ActiveTime: 5 * time.Second, ModelTimeout: time.Second, OutputTokens: 256})
	defer func() { stop(); workers.Wait() }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		d, err := a.store.Detail(r.TaskID, true)
		if err != nil {
			t.Fatal(err)
		}
		if d.Task.Status == "waiting_user" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture did not stop waiting for input")
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	workers.Wait()
	prefix := "/api/v1/conversations/" + r.ConversationID
	body := `{"client_request_id":"read-only-task-tag","text":"普通备注","task_ref":{"task_id":"` + r.TaskID + `"}}`
	if code, response := a.post(prefix+"/messages", body); code != http.StatusAccepted || response["model_pending"] != true {
		t.Fatalf("task ref was not durably admitted: %d %+v", code, response)
	}
	if err := a.store.ProcessChatAdmissions(ctx, nil); err != nil {
		t.Fatal(err)
	}
	res, err := a.ts.Client().Do(a.request(http.MethodGet, prefix+"/history", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var page work.MessagePage
	if err := json.NewDecoder(res.Body).Decode(&page); err != nil || len(page.Items) != 3 || page.Items[2].TaskRef == nil || page.Items[2].TaskRef.TaskID != r.TaskID || page.Items[1].TaskRef != nil {
		t.Fatalf("wrong explicit task reference in history: %+v %v", page, err)
	}
	d, err := a.store.Detail(r.TaskID, true)
	if err != nil || d.Task.Status != "waiting_user" || d.Run.TaskID != "" {
		t.Fatalf("reference changed actual current task: %+v %v", d, err)
	}
}
