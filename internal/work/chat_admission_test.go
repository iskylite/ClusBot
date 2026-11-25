package work

// A1 验收：模型可用路径下，受理与提交链的关联。
// 分类/任务已提交时重试只返回原结果；受理回执关联到后续 Run/Task。

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestChatAdmissionUnreadyRouterDoesNotBlockWorkerReceipts(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "roles.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for n := range 70 {
		if _, err := s.AdmitChat(ctx, fmt.Sprintf("pending-router-%d", n), "user:fixture", "", "等待路由", "", false, Delivery{}, nil, false); err != nil {
			t.Fatal(err)
		}
	}
	const workerKey = "pending-ready-worker"
	if _, err := s.AdmitChat(ctx, workerKey, "user:fixture", "", "明确委托", "", true, Delivery{}, nil, false); err != nil {
		t.Fatal(err)
	}
	router := func(context.Context, string, []Message, []RouteCandidate, int) (DelegationResult, error) {
		t.Fatal("Worker-only admission unexpectedly called Router")
		return DelegationResult{}, nil
	}
	ready := func(_ context.Context, role string) bool { return role == TaskWorkerModelRole }
	var cursor chatAdmissionCursor
	for range 2 {
		if err := s.processChatAdmissions(ctx, router, ready, &cursor); err != nil {
			t.Fatal(err)
		}
	}
	receipt, err := s.ReadAdmission(ctx, workerKey)
	if err != nil || receipt.State != "applied" || receipt.TaskID == "" || receipt.RunID == "" {
		t.Fatalf("older Router receipts blocked Worker: %+v %v", receipt, err)
	}
	receipt, err = s.ReadAdmission(ctx, "pending-router-0")
	if err != nil || receipt.State != "received" || receipt.RunID != "" || receipt.Reason != ChatModelPendingReason(GeneralRouterModelRole) {
		t.Fatalf("unavailable Router was not preserved: %+v %v", receipt, err)
	}
}

func TestChatAdmissionLocalSelectionFailureCanRetryWithoutReplayingModel(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "selection-race.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const key = "router-selection-race"
	if _, err := s.AdmitChat(ctx, key, "user:fixture", "", "解释请求", "", false, Delivery{}, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := s.ProcessChatAdmissions(ctx, func(context.Context, string, []Message, []RouteCandidate, int) (DelegationResult, error) {
		return DelegationResult{}, ErrGeneralRouterRoleUnavailable
	}); err != nil {
		t.Fatal(err)
	}
	var reserved int
	if err := s.db.QueryRow(`SELECT count(*) FROM submission_classifications WHERE request_key=?`, key).Scan(&reserved); err != nil || reserved != 0 {
		t.Fatalf("pre-egress failure left uncertain reservation: %d %v", reserved, err)
	}
	if _, err := s.db.Exec(`UPDATE inbox_events SET next_retry_at=0 WHERE source='chat' AND event_id=?`, key); err != nil {
		t.Fatal(err)
	}
	var calls int
	route := func(context.Context, string, []Message, []RouteCandidate, int) (DelegationResult, error) {
		calls++
		return DelegationResult{Route: &RouteDecision{Kind: "chat"}, Requests: 1}, nil
	}
	for range 2 {
		if err := s.ProcessChatAdmissions(ctx, route); err != nil {
			t.Fatal(err)
		}
	}
	receipt, err := s.ReadAdmission(ctx, key)
	if err != nil || calls != 1 || receipt.State != "applied" || receipt.RunID == "" {
		t.Fatalf("recovered receipt=%+v calls=%d err=%v", receipt, calls, err)
	}
	const dispatched = "router-dispatched-without-outcome"
	if _, err := s.AdmitChat(ctx, dispatched, "user:fixture", "", "已经计入一次请求", "", false, Delivery{}, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := s.ProcessChatAdmissions(ctx, func(context.Context, string, []Message, []RouteCandidate, int) (DelegationResult, error) {
		return DelegationResult{Requests: 1}, ErrGeneralRouterRoleUnavailable
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.ProcessChatAdmissions(ctx, route); err != nil {
		t.Fatal(err)
	}
	receipt, err = s.ReadAdmission(ctx, dispatched)
	if err != nil || receipt.State != "failed" || calls != 1 {
		t.Fatalf("a pre-dispatch reason retried an already-issued call: receipt=%+v calls=%d err=%v", receipt, calls, err)
	}
}

func TestA1AdmissionLinkedAfterSubmit(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "a1-link.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.configureRuntime(1, RunLimits{ActiveTime: time.Second, ModelTimeout: time.Second, OutputTokens: 80})
	ctx := context.Background()

	// 1. 模型可用前先持久受理（与 HTTP 提交链一致）。
	admission, err := s.AdmitChat(ctx, "a1-link-1", "user:session", "", "整理季度汇报", "", false, Delivery{}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if admission.Receipt.State != "received" || admission.ModelPending {
		t.Fatalf("admission: %+v (模型可用不应挂起)", admission)
	}

	// 2. 分类并提交（模型可用路径）。
	classify := func(_ context.Context, _ string, _ []Message, limit int) (DelegationResult, error) {
		return DelegationResult{Task: false, Requests: 1}, nil
	}
	run, err := s.SubmitClassifiedChat(ctx, "a1-link-1", "整理季度汇报", "", "", Delivery{}, classify)
	if err != nil {
		t.Fatal(err)
	}

	// 3. 受理回执已关联到 Run 并标记已处理。
	receipt, err := s.ReadAdmission(ctx, "a1-link-1")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.State != "applied" {
		t.Errorf("admission state=%s, want applied (提交后关联)", receipt.State)
	}

	// 4. 分类/任务已提交时重试同键：只返回原结果（不重复分类/建任务）。
	retry, err := s.SubmitClassifiedChat(ctx, "a1-link-1", "整理季度汇报", "", "", Delivery{}, classify)
	if err != nil {
		t.Fatal(err)
	}
	if retry.ID != run.ID {
		t.Errorf("retry run=%s, want original %s (重试只返回原结果)", retry.ID, run.ID)
	}
}

func TestA1AdmitChatIdempotentAndConflict(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "a1-idem.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	// 同键同请求：返回原回执。
	a1, err := s.AdmitChat(ctx, "a1-idem-1", "user:session", "", "整理周报", "", false, Delivery{}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := s.AdmitChat(ctx, "a1-idem-1", "user:session", "", "整理周报", "", false, Delivery{}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !a2.Receipt.Duplicate || a2.Receipt.ReceiptID != a1.Receipt.ReceiptID {
		t.Errorf("same key not idempotent: %+v vs %+v", a1.Receipt, a2.Receipt)
	}

	// 同键异请求：冲突。
	if _, err := s.AdmitChat(ctx, "a1-idem-1", "user:session", "", "不同内容", "", false, Delivery{}, nil, true); err == nil {
		t.Error("same key different content not conflicted")
	}
}

func TestA1RestartResubmitsFullChatInput(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "replay.db")
	ctx := t.Context()
	initial, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	const text = "先整理周报。\n保留这一行的补充说明。"
	if _, err := initial.AdmitChat(ctx, "restart-text", "user:session", "", text, "", false, Delivery{}, nil, false); err != nil {
		initial.Close()
		t.Fatal(err)
	}
	if err := initial.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.RecoverRuntime(ctx, now()); err != nil {
		t.Fatal(err)
	}
	calls := 0
	route := func(_ context.Context, input string, _ []Message, _ []RouteCandidate, _ int) (DelegationResult, error) {
		calls++
		if input != text {
			t.Errorf("router input: %q", input)
		}
		return DelegationResult{Route: &RouteDecision{Kind: "chat"}}, nil
	}
	for range 2 {
		if err := s.ProcessChatAdmissions(ctx, route); err != nil {
			t.Fatal(err)
		}
	}
	receipt, err := s.ReadAdmission(ctx, "restart-text")
	if err != nil || calls != 1 || receipt.State != "applied" || receipt.ConversationID == "" {
		t.Fatalf("receipt: %+v, calls: %d, err: %v", receipt, calls, err)
	}
	detail, err := s.Detail(receipt.ConversationID, false)
	if err != nil || len(detail.Messages) != 1 || detail.Messages[0].Text != text {
		t.Fatalf("restored messages: %+v %v", detail.Messages, err)
	}
}
