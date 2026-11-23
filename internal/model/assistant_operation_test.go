package model

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"clusbot/internal/work"
)

func TestAssistantOperationDoesNotFallbackToAnotherProvider(t *testing.T) {
	var primaryCalls, fallbackCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer fallback.Close()
	_, settings, first, second := routingFixture(t, primary.URL, fallback.URL)
	if _, err := settings.SaveModelRoutingPolicy(t.Context(), "task", work.TaskWorkerModelRole, routingInput(0, first, second)); err != nil {
		t.Fatal(err)
	}
	evidence := AssistantOperationEvidence{
		Scope: "pod", Focus: "node_investigation", TotalNodes: 1, Health: work.ClusterHealthCounts{Unknown: 1}, ComputedAt: time.Now().UnixMilli(),
	}
	invalid := evidence
	invalid.Scope = "/private/context"
	_, err := settings.RecommendAssistantOperation(t.Context(), invalid, func(context.Context) error { t.Fatal("invalid evidence reached export guard"); return nil })
	if err == nil || primaryCalls.Load() != 0 {
		t.Fatal("unrestricted evidence reached a provider")
	}
	denied := errors.New("export revoked")
	_, err = settings.RecommendAssistantOperation(t.Context(), evidence, func(context.Context) error { return denied })
	if !errors.Is(err, denied) || primaryCalls.Load() != 0 || fallbackCalls.Load() != 0 {
		t.Fatalf("rejected export reached a provider: primary=%d fallback=%d err=%v", primaryCalls.Load(), fallbackCalls.Load(), err)
	}
	_, err = settings.RecommendAssistantOperation(t.Context(), evidence, func(context.Context) error { return nil })
	if err == nil || primaryCalls.Load() != 1 || fallbackCalls.Load() != 0 {
		t.Fatalf("model request retried or changed provider: primary=%d fallback=%d err=%v", primaryCalls.Load(), fallbackCalls.Load(), err)
	}
}
