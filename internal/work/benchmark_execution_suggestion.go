package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

const benchmarkExecutionSuggestedEvent = "benchmark.execution_suggested"

type BenchmarkExecutionSuggestion struct {
	TaskID       string `json:"task_id"`
	StepID       string `json:"step_id"`
	CaseKey      string `json:"case_key"`
	PlanRevision int    `json:"plan_revision"`
	State        string `json:"state"`
}

type BenchmarkExecutionCandidate struct {
	CaseKey  string `json:"case_key"`
	Position int    `json:"position"`
}

type BenchmarkExecutionModelInput struct {
	Text  string                        `json:"text"`
	Cases []BenchmarkExecutionCandidate `json:"cases"`
}

type BenchmarkExecutionModel func(context.Context, BenchmarkExecutionModelInput, int) (string, error)

func canInterpretExecution(text string) bool {
	text = strings.TrimSpace(strings.ToLower(text))
	if text == "" || len(text) > 1024 || strings.ContainsAny(text, "\n\r?？`\x22'“”「」") ||
		strings.HasPrefix(text, "[") || strings.HasPrefix(text, ">") || strings.HasPrefix(text, "/") || strings.HasPrefix(text, "$") || strings.HasSuffix(text, "吗") {
		return false
	}
	for _, denied := range []string{"先别", "先不", "暂缓", "暂时", "不要跳过", "别跳过", "不用跳过", "不要略过", "不要不跑", "不跳过", "取消跳过", "免测", "不验收", "修改默认", "更新默认", "改成默认", "永久", "以后都", "顺便", "另外", "日志", "示例", "假如", "如果", "假设", "是否", "能否", "能不能", "可不可以", "怎么", "如何", "what if", "should ", "don't skip", "do not skip"} {
		if strings.Contains(text, denied) {
			return false
		}
	}
	return true
}

func parseExecutionSuggestion(raw, text string) string {
	if len(raw) > 4096 {
		return ""
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	if token, err := dec.Token(); err != nil || token != json.Delim('{') {
		return ""
	}
	fields := map[string]string{}
	for dec.More() {
		token, err := dec.Token()
		key, ok := token.(string)
		if err != nil || !ok || key != "action" && key != "case_key" && key != "quote" {
			return ""
		}
		if _, exists := fields[key]; exists {
			return ""
		}
		var value *string
		if err := dec.Decode(&value); err != nil || value == nil {
			return ""
		}
		fields[key] = *value
	}
	if token, err := dec.Token(); err != nil || token != json.Delim('}') {
		return ""
	}
	if _, err := dec.Token(); err != io.EOF {
		return ""
	}
	if len(fields) != 3 || fields["action"] != "suggest_skip_this_run" || fields["quote"] != text || !validBenchKey(fields["case_key"]) {
		return ""
	}
	return fields["case_key"]
}

func executionCandidates(ctx context.Context, tx *sql.Tx, actor string, target BenchmarkExecutionTarget) ([]BenchmarkExecutionCandidate, error) {
	if err := checkBenchmarkRevisionTarget(ctx, tx, actor, BenchmarkRevisionTarget{target.TaskID, target.ExpectedPlanRevision}); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT b.case_key,s.position FROM benchmark_cases b JOIN steps s ON s.step_id=b.step_id AND s.task_id=b.task_id
WHERE b.task_id=? AND b.phase='queued' AND b.operation_id=''
AND NOT EXISTS(SELECT 1 FROM operations o WHERE o.task_id=b.task_id AND o.step_id=b.step_id)
AND NOT EXISTS(SELECT 1 FROM benchmark_target_holds h WHERE h.step_id=b.step_id)
AND NOT EXISTS(SELECT 1 FROM json_each(b.frozen_targets) ref
 LEFT JOIN ops_assets a ON a.object_ref=ref.value->>'ref' AND a.revision=ref.value->>'revision'
  AND a.ownership_epoch=ref.value->>'epoch'
 LEFT JOIN authorizations auth ON auth.auth_id=json_extract(b.authorization_refs,'$.'||json_quote(ref.value->>'ref'))
  AND auth.subject=? AND auth.action='benchmark.execute' AND auth.object_ref=a.object_ref
  AND auth.revoked_at=0 AND auth.valid_from<=? AND (auth.expires_at=0 OR auth.expires_at>?)
 WHERE a.object_ref IS NULL OR auth.auth_id IS NULL)
ORDER BY s.position,b.step_id LIMIT 65`, target.TaskID, actor, now(), now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cases := []BenchmarkExecutionCandidate{}
	for rows.Next() {
		var c BenchmarkExecutionCandidate
		if err := rows.Scan(&c.CaseKey, &c.Position); err != nil {
			return nil, err
		}
		cases = append(cases, c)
	}
	if len(cases) == 0 || len(cases) > 64 {
		return nil, ErrBenchmarkExecutionClarify
	}
	return cases, rows.Err()
}

func readBenchmarkExecutionSuggestion(tx *sql.Tx, run string) (*BenchmarkExecutionSuggestion, error) {
	var raw string
	err := tx.QueryRow("SELECT data FROM run_events WHERE run_id=? AND kind=?", run, benchmarkExecutionSuggestedEvent).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var suggestion BenchmarkExecutionSuggestion
	if err := json.Unmarshal([]byte(raw), &suggestion); err != nil {
		return nil, err
	}
	return &suggestion, nil
}

// SuggestBenchmarkExecutionChat permits model interpretation, not model
// authorization. Only a later explicit click can change execution.
func (s *Store) SuggestBenchmarkExecutionChat(ctx context.Context, request, actor, conversation, text, scope string, target BenchmarkExecutionTarget, interpret BenchmarkExecutionModel) (Run, error) {
	text = strings.TrimSpace(text)
	if actor == "" || conversation == "" || target.TaskID == "" || target.ExpectedPlanRevision < 1 ||
		len(request) < 8 || len(request) > 128 || strings.TrimSpace(request) != request {
		return Run{}, ErrBenchmarkInput
	}
	if !canInterpretExecution(text) {
		return Run{}, ErrBenchmarkExecutionClarify
	}
	fingerprint := digest(toJSON([]any{"benchmark-execution-model-suggestion", actor, conversation, text, scope, target}))
	var candidates []BenchmarkExecutionCandidate
	load := func() (Run, error) {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return Run{}, err
		}
		defer tx.Rollback()
		if existing, err := previousSubmission(tx, request, fingerprint); err != nil || existing.ID != "" {
			return existing, err
		}
		if err := checkConversationSubmission(tx, conversation); err != nil {
			return Run{}, err
		}
		candidates, err = executionCandidates(ctx, tx, actor, target)
		return Run{}, err
	}
	if prior, err := load(); err != nil || prior.ID != "" {
		return prior, err
	}
	if interpret == nil {
		return Run{}, ErrClassification
	}
	classified, err := s.classifySubmission(ctx, request, fingerprint, conversation, text, nil, func(call context.Context, current string, _ []Message, limit int) (DelegationResult, error) {
		raw, err := interpret(call, BenchmarkExecutionModelInput{Text: current, Cases: candidates}, limit)
		if err != nil {
			return DelegationResult{Requests: 1}, errors.New("默认模型未能生成执行建议")
		}
		result := DelegationResult{Requests: 1, Route: &RouteDecision{Kind: "benchmark_execution_clarify"}}
		key := parseExecutionSuggestion(raw, current)
		for _, candidate := range candidates {
			if candidate.CaseKey == key {
				result.Route = &RouteDecision{Kind: "benchmark_execution_suggestion", Goal: key}
				break
			}
		}
		return result, nil
	})
	if err != nil {
		return Run{}, err
	}
	if classified.Route == nil || classified.Route.Kind != "benchmark_execution_suggestion" {
		return Run{}, ErrBenchmarkExecutionClarify
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	if existing, err := previousSubmission(tx, request, fingerprint); err != nil || existing.ID != "" {
		return existing, err
	}
	if err := checkConversationSubmission(tx, conversation); err != nil {
		return Run{}, err
	}
	fresh, err := executionCandidates(ctx, tx, actor, target)
	if err != nil {
		return Run{}, err
	}
	found := false
	for _, c := range fresh {
		found = found || c.CaseKey == classified.Route.Goal
	}
	if !found {
		return Run{}, ErrBenchmarkCaseNotPending
	}
	var step string
	err = tx.QueryRowContext(ctx, `SELECT step_id FROM benchmark_cases
WHERE task_id=? AND case_key=? AND phase='queued' AND operation_id=''`, target.TaskID, classified.Route.Goal).Scan(&step)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrBenchmarkCaseNotPending
	}
	if err != nil {
		return Run{}, err
	}
	if _, err := admitChatInputWithFingerprint(tx, request, actor, text, fingerprint, "", now()); err != nil {
		return Run{}, err
	}
	run, err := submitWorkTx(ctx, tx, request, fingerprint, text, scope, conversation, false, Delivery{}, &classified, nil)
	if err != nil {
		return Run{}, err
	}
	suggestion := BenchmarkExecutionSuggestion{TaskID: target.TaskID, StepID: step, CaseKey: classified.Route.Goal, PlanRevision: target.ExpectedPlanRevision, State: "confirmation_required"}
	if err := addEvent(tx, run.ID, benchmarkExecutionSuggestedEvent, suggestion); err != nil {
		return Run{}, err
	}
	if err := linkAdmissionToRun(tx, request, run.ID, "", now()); err != nil {
		return Run{}, err
	}
	run.BenchmarkExecutionSuggestion = &suggestion
	return run, tx.Commit()
}

func (s *Store) ConfirmBenchmarkExecutionSuggestion(ctx context.Context, actor, conversation, run, action string) (BenchmarkExecutionControlReceipt, error) {
	if actor == "" || conversation == "" || run == "" || action != benchmarkSkipThisRun {
		return BenchmarkExecutionControlReceipt{}, ErrBenchmarkInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BenchmarkExecutionControlReceipt{}, err
	}
	defer tx.Rollback()
	var archived bool
	if err := tx.QueryRowContext(ctx, `SELECT c.archived FROM runs r JOIN conversations c ON c.id=r.conversation_id
WHERE r.id=? AND r.conversation_id=? AND r.status='completed' AND r.task_id IS NULL
AND EXISTS(SELECT 1 FROM inbox_events i JOIN submissions sub ON sub.request_key=i.event_id
WHERE i.source='chat' AND i.run_id=r.id AND i.state='applied' AND i.principal=? AND sub.run_id=r.id)`,
		run, conversation, actor).Scan(&archived); err != nil {
		return BenchmarkExecutionControlReceipt{}, err
	}
	suggestion, err := readBenchmarkExecutionSuggestion(tx, run)
	if err != nil {
		return BenchmarkExecutionControlReceipt{}, err
	}
	if suggestion == nil || suggestion.State != "confirmation_required" {
		return BenchmarkExecutionControlReceipt{}, sql.ErrNoRows
	}
	if prior, err := readBenchmarkExecutionChatReceipt(tx, run); err != nil {
		return BenchmarkExecutionControlReceipt{}, err
	} else if prior != nil {
		return *prior, nil
	}
	if archived {
		return BenchmarkExecutionControlReceipt{}, ErrConversationArchived
	}
	request := benchmarkChatControlPrefix + digest("confirmed-suggestion:" + run)[:32]
	receipt, err := applyBenchmarkExecutionControlTx(ctx, tx, actor, suggestion.TaskID, suggestion.StepID, BenchmarkExecutionControlInput{
		Action: benchmarkSkipThisRun, ExpectedPlanRevision: suggestion.PlanRevision, RequestID: request, Reason: "用户确认仅跳过本次执行",
	})
	if err != nil {
		return BenchmarkExecutionControlReceipt{}, err
	}
	if err := addEvent(tx, run, benchmarkExecutionSkippedEvent, receipt); err != nil {
		return BenchmarkExecutionControlReceipt{}, err
	}
	return receipt, tx.Commit()
}
