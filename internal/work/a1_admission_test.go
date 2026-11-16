package work

// A1 验收：模型可用路径下，受理与提交链的关联。
// 分类/任务已提交时重试只返回原结果；受理回执关联到后续 Run/Task。

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

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
	if _, err := s.RecoverV3(ctx, now()); err != nil {
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
