package work

import "testing"

func TestExecutionSuggestionStrictModelAndIntentEnvelope(t *testing.T) {
	const text = "这次不跑集合通信，默认不变"
	for _, unsafe := range []string{
		"集合通信可以不跑吗？", "如果这次不跑集合通信", "不要跳过集合通信", "集合通信先别跑", "「这次不跑集合通信」",
		"免测集合通信", "这次不跑集合通信，修改默认", "不跑集合通信顺便取消另一个任务", "[INFO] 这次不跑集合通信", "如何跳过集合通信",
	} {
		if canInterpretExecution(unsafe) {
			t.Fatalf("unsafe input sent to the selector: %q", unsafe)
		}
	}
	if !canInterpretExecution(text) {
		t.Fatal("a bounded natural request could not be interpreted")
	}
	for _, mock := range []string{
		`{"action":"suggest_skip_this_run","case_key":"collective","quote":"这次不跑集合通信"}`,
		`{"action":"suggest_skip_this_run","case_key":"collective","case_key":"other","quote":"这次不跑集合通信，默认不变"}`,
		`{"action":"suggest_skip_this_run","case_key":"collective","quote":"这次不跑集合通信，默认不变","asset":"node-1"}`,
		`{"action":"skip_this_run","case_key":"collective","quote":"这次不跑集合通信，默认不变"}`,
		`{"action":"suggest_skip_this_run","case_key":"collective","quote":"这次不跑集合通信，默认不变"} {}`,
		`{"action":"clarify"}`,
	} {
		if result := parseExecutionSuggestion(mock, text); result != "" {
			t.Fatalf("malformed model result became a case: %q", result)
		}
	}
	candidate := parseExecutionSuggestion(`{"action":"suggest_skip_this_run","case_key":"collective","quote":"这次不跑集合通信，默认不变"}`, text)
	if candidate != "collective" {
		t.Fatalf("exact proposal not accepted: %q", candidate)
	}
}
