package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
)

const benchmarkExecutionSkippedEvent = "benchmark.execution_skipped"

var ErrBenchmarkExecutionClarify = errors.New("未执行。请明确本次要跳过的一个测项，例如「本次跳过第二项」或「本次跳过 single-node」")

type BenchmarkExecutionTarget struct {
	TaskID               string `json:"task_id"`
	ExpectedPlanRevision int    `json:"expected_plan_revision"`
}

var benchmarkSkipCaseText = regexp.MustCompile(`^(?:请)?本次跳过[ \t]+([A-Za-z0-9_.-]{1,128})[。！!]?$`)
var benchmarkSkipOrdinalText = regexp.MustCompile(`^(?:请)?本次跳过[ \t]*第([0-9]{1,2}|一|二|三|四|五|六|七|八|九|十)项[。！!]?$`)

func benchmarkSkipSelector(text string) (string, int, error) {
	if m := benchmarkSkipOrdinalText.FindStringSubmatch(strings.TrimSpace(text)); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 && n <= 64 {
			return "", n, nil
		}
		for i, numeral := range []string{"一", "二", "三", "四", "五", "六", "七", "八", "九", "十"} {
			if m[1] == numeral {
				return "", i + 1, nil
			}
		}
	}
	if m := benchmarkSkipCaseText.FindStringSubmatch(strings.TrimSpace(text)); m != nil && validBenchKey(m[1]) {
		return m[1], 0, nil
	}
	return "", 0, ErrBenchmarkExecutionClarify
}

func readBenchmarkExecutionChatReceipt(tx *sql.Tx, run string) (*BenchmarkExecutionControlReceipt, error) {
	var raw string
	err := tx.QueryRow("SELECT data FROM run_events WHERE run_id=? AND kind=?", run, benchmarkExecutionSkippedEvent).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var result BenchmarkExecutionControlReceipt
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// SubmitBenchmarkExecutionChat is an explicit, task-tagged one-case command.
// It cannot guess the task, infer a waiver, invoke a model or stop a job.
func (s *Store) SubmitBenchmarkExecutionChat(ctx context.Context, request, actor, conversation, text, scope string, target BenchmarkExecutionTarget) (Run, error) {
	if actor == "" || conversation == "" || target.TaskID == "" || target.ExpectedPlanRevision < 1 ||
		len(request) < 8 || len(request) > 128 || strings.TrimSpace(request) != request || len(text) > 512 {
		return Run{}, ErrBenchmarkInput
	}
	key, position, err := benchmarkSkipSelector(text)
	if err != nil {
		return Run{}, err
	}
	fingerprint := digest(toJSON([]any{"benchmark-execution-intent", actor, conversation, text, scope, target}))
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
	var current int
	if err := tx.QueryRowContext(ctx, "SELECT plan_revision FROM benchmark_runs WHERE task_id=? AND actor=?", target.TaskID, actor).Scan(&current); err != nil {
		return Run{}, err
	}
	if current != target.ExpectedPlanRevision {
		return Run{}, ErrTaskRevision
	}
	var step string
	// An ordinal always refers to the current displayed plan order, including
	// already dispatched and skipped steps, never to a filtered candidate list.
	err = tx.QueryRowContext(ctx, `SELECT min(b.step_id) FROM benchmark_cases b JOIN steps st ON st.step_id=b.step_id AND st.task_id=b.task_id
WHERE b.task_id=? AND ((?<>'' AND b.case_key=?) OR (? >0 AND st.position=?))
HAVING count(*)=1`, target.TaskID, key, key, position, position).Scan(&step)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrBenchmarkExecutionClarify
	}
	if err != nil {
		return Run{}, err
	}
	controlRequest := benchmarkChatControlPrefix + digest(toJSON([]string{actor, conversation, request}))[:32]
	receipt, err := applyBenchmarkExecutionControlTx(ctx, tx, actor, target.TaskID, step, BenchmarkExecutionControlInput{
		Action: benchmarkSkipThisRun, ExpectedPlanRevision: target.ExpectedPlanRevision, RequestID: controlRequest, Reason: "用户在对话中明确要求仅本次跳过",
	})
	if err != nil {
		return Run{}, err
	}
	if _, err := admitChatInputWithFingerprint(tx, request, actor, text, fingerprint, "", now()); err != nil {
		return Run{}, err
	}
	c := submissionClassification{DelegationResult: DelegationResult{Route: &RouteDecision{Kind: "benchmark_execution_skip"}}}
	run, err := submitWorkTx(ctx, tx, request, fingerprint, text, scope, conversation, false, Delivery{}, &c, nil)
	if err != nil {
		return Run{}, err
	}
	if err := addEvent(tx, run.ID, benchmarkExecutionSkippedEvent, receipt); err != nil {
		return Run{}, err
	}
	if err := linkAdmissionToRun(tx, request, run.ID, "", now()); err != nil {
		return Run{}, err
	}
	run.BenchmarkExecutionControl = &receipt
	return run, tx.Commit()
}
