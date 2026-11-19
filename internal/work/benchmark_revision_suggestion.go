package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

const benchmarkRevisionSuggestionEvent = "benchmark.revision_suggested"

type BenchmarkRevisionTarget struct {
	TaskID           string `json:"task_id"`
	ExpectedRevision int    `json:"expected_revision"`
}

func revisionSuggestionText(text string) bool {
	text = strings.TrimSpace(strings.ToLower(text))
	if text == "" || len(text) > 4096 || strings.HasPrefix(text, "/") ||
		strings.HasPrefix(text, "[") || strings.HasPrefix(text, ">") || strings.HasPrefix(text, "$") ||
		strings.ContainsAny(text, "\n\r?？`'\x22“”「」") {
		return false
	}
	for _, s := range []string{"日志", "示例", "是否", "能否", "能不能", "可以吗", "怎么", "如何", "假设", "不要改", "别改", "先不改", "不需要改", "不用修改", "不要修改", "不要预演", "别预演", "查进度", "看进度", "查状态", "what if", "how to", "do not change"} {
		if strings.Contains(text, s) {
			return false
		}
	}
	return true
}

func checkBenchmarkRevisionTarget(ctx context.Context, tx *sql.Tx, actor string, target BenchmarkRevisionTarget) error {
	var version int
	var editable bool
	if err := tx.QueryRowContext(ctx, `SELECT b.plan_revision,
NOT b.stop_requested AND t.state IN ('pending','running','paused') AND EXISTS(
 SELECT 1 FROM benchmark_cases c WHERE c.task_id=b.task_id AND c.phase='queued' AND c.operation_id='')
FROM benchmark_runs b JOIN task_states t ON t.task_id=b.task_id
WHERE b.task_id=? AND b.actor=?`, target.TaskID, actor).Scan(&version, &editable); err != nil {
		return err
	}
	if version != target.ExpectedRevision {
		return ErrTaskRevision
	}
	if !editable {
		return ErrState
	}
	return nil
}

func readBenchmarkRevisionSuggestion(tx *sql.Tx, run string) (*BenchmarkRevisionTarget, error) {
	var raw string
	err := tx.QueryRow(`SELECT data FROM run_events WHERE run_id=? AND kind=?`, run, benchmarkRevisionSuggestionEvent).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var target BenchmarkRevisionTarget
	if err := json.Unmarshal([]byte(raw), &target); err != nil {
		return nil, err
	}
	return &target, nil
}

// SubmitBenchmarkRevisionSuggestion binds an explicitly labelled message
// to one revision. It is not an instruction to mutate, dispatch or ask a model.
func (s *Store) SubmitBenchmarkRevisionSuggestion(ctx context.Context, key, actor, conversation, text, scope string, target BenchmarkRevisionTarget) (Run, error) {
	if actor == "" || conversation == "" || target.TaskID == "" || target.ExpectedRevision < 1 || !revisionSuggestionText(text) {
		return Run{}, ErrBenchmarkInput
	}
	fingerprint := digest(toJSON([]any{"benchmark-revision-label", actor, conversation, text, scope, target}))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	if existing, err := previousSubmission(tx, key, fingerprint); err != nil || existing.ID != "" {
		return existing, err
	}
	if err := checkBenchmarkRevisionTarget(ctx, tx, actor, target); err != nil {
		return Run{}, err
	}
	if _, err := admitChatInputWithFingerprint(tx, key, actor, text, fingerprint, "", now()); err != nil {
		return Run{}, err
	}
	c := submissionClassification{DelegationResult: DelegationResult{Route: &RouteDecision{Kind: "benchmark_revision_suggestion"}}}
	run, err := submitWorkTx(ctx, tx, key, fingerprint, text, scope, conversation, false, Delivery{}, &c, nil)
	if err != nil {
		return Run{}, err
	}
	if err := addEvent(tx, run.ID, benchmarkRevisionSuggestionEvent, target); err != nil {
		return Run{}, err
	}
	if err := linkAdmissionToRun(tx, key, run.ID, "", now()); err != nil {
		return Run{}, err
	}
	run.BenchmarkRevisionSuggestion = &target
	return run, tx.Commit()
}

func (s *Store) resolveBenchmarkRevisionSuggestion(ctx context.Context, actor, conversation, run string) (BenchmarkRevisionTarget, string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BenchmarkRevisionTarget{}, "", err
	}
	defer tx.Rollback()
	var text string
	err = tx.QueryRowContext(ctx, `SELECT m.text FROM runs r JOIN messages m ON m.run_id=r.id AND m.conversation_id=r.conversation_id AND m.role='user'
WHERE r.id=? AND r.conversation_id=? AND r.status='completed' AND r.task_id IS NULL
AND EXISTS (SELECT 1 FROM inbox_events i JOIN submissions sub ON sub.request_key=i.event_id
 WHERE i.source='chat' AND i.run_id=r.id AND i.state='applied' AND i.principal=? AND sub.run_id=r.id)
ORDER BY m.seq LIMIT 1`, run, conversation, actor).Scan(&text)
	if err != nil {
		return BenchmarkRevisionTarget{}, "", err
	}
	target, err := readBenchmarkRevisionSuggestion(tx, run)
	if err != nil {
		return BenchmarkRevisionTarget{}, "", err
	}
	if target == nil || !revisionSuggestionText(text) {
		return BenchmarkRevisionTarget{}, "", sql.ErrNoRows
	}
	return *target, text, nil
}
