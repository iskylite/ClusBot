package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const benchmarkRevisionChatEvent = "benchmark.revision_preview"
const benchmarkRevisionAppliedEvent = "benchmark.revision_applied"

type benchmarkRevisionAppliedReceipt struct {
	TaskID   string `json:"task_id"`
	Revision int    `json:"revision"`
}

type BenchmarkRevisionChatInput struct {
	BenchmarkRevisionAIInput
	SourceRunID string `json:"source_run_id,omitempty"`
	Action      string `json:"action"`
	TaskID      string `json:"task_id"`
	RequestID   string `json:"client_request_id"`
}

type BenchmarkRevisionChatReceipt struct {
	RunID          string                    `json:"run_id"`
	ConversationID string                    `json:"conversation_id"`
	TaskID         string                    `json:"task_id"`
	SourceRunID    string                    `json:"source_run_id,omitempty"`
	Proposal       BenchmarkRevisionProposal `json:"proposal"`
}

type BenchmarkRevisionChatMetadata struct {
	TaskID           string `json:"task_id"`
	SourceRunID      string `json:"source_run_id,omitempty"`
	ExpectedRevision int    `json:"expected_revision"`
	AppliedRevision  int    `json:"applied_revision,omitempty"`
}

// Metadata does not reopen an expired preview. It only restores the source
// identity and any already-recorded confirmation for its authorized owner.
func (s *Store) ReadBenchmarkRevisionChatMetadata(ctx context.Context, actor, conversation, run string) (BenchmarkRevisionChatMetadata, error) {
	recorded, err := checkedRecordedBenchmarkPreview(s.db, actor, conversation, run)
	if err != nil {
		return BenchmarkRevisionChatMetadata{}, err
	}
	out := BenchmarkRevisionChatMetadata{TaskID: recorded.TaskID, SourceRunID: recorded.SourceRunID, ExpectedRevision: recorded.Proposal.ExpectedRevision}
	var raw string
	err = s.db.QueryRowContext(ctx, `SELECT data FROM run_events WHERE run_id=? AND kind=?`, run, benchmarkRevisionAppliedEvent).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return BenchmarkRevisionChatMetadata{}, err
	}
	var applied benchmarkRevisionAppliedReceipt
	if err := json.Unmarshal([]byte(raw), &applied); err != nil {
		return BenchmarkRevisionChatMetadata{}, err
	}
	if applied.TaskID != recorded.TaskID || applied.Revision != out.ExpectedRevision+1 {
		return BenchmarkRevisionChatMetadata{}, ErrState
	}
	out.AppliedRevision = applied.Revision
	return out, nil
}

func readBenchmarkRevisionChat(q recordsQuerier, actor, conversation, run string) (BenchmarkRevisionChatReceipt, error) {
	var raw string
	err := q.QueryRow(`SELECT e.data FROM run_events e JOIN runs r ON r.id=e.run_id
JOIN benchmark_runs b ON b.task_id=json_extract(e.data,'$.task_id') AND b.actor=?
WHERE r.conversation_id=? AND r.id=? AND e.kind=?`, actor, conversation, run, benchmarkRevisionChatEvent).Scan(&raw)
	if err != nil {
		return BenchmarkRevisionChatReceipt{}, err
	}
	var out BenchmarkRevisionChatReceipt
	err = json.Unmarshal([]byte(raw), &out)
	return out, err
}

// SubmitBenchmarkRevisionChat is an explicit conversation action, not a
// matcher for arbitrary messages. Only the reviewed preview is archived;
// the selected benchmark remains unchanged and is never dispatched here.
func (s *Store) SubmitBenchmarkRevisionChat(ctx context.Context, actor, conversation string, input BenchmarkRevisionChatInput,
	provider BenchmarkProvider, interpret BenchmarkRevisionModel) (BenchmarkRevisionChatReceipt, error) {
	if input.Action != "preview_benchmark_revision" || conversation == "" || len(input.RequestID) < 8 || len(input.RequestID) > 128 {
		return BenchmarkRevisionChatReceipt{}, ErrBenchmarkInput
	}
	if input.SourceRunID != "" {
		if input.TaskID != "" || input.ExpectedRevision != 0 || input.Text != "" || input.AllowExpectedActualChange {
			return BenchmarkRevisionChatReceipt{}, ErrBenchmarkInput
		}
		target, text, err := s.resolveBenchmarkRevisionSuggestion(ctx, actor, conversation, input.SourceRunID)
		if err != nil {
			return BenchmarkRevisionChatReceipt{}, err
		}
		input.TaskID, input.ExpectedRevision, input.Text = target.TaskID, target.ExpectedRevision, text
	}
	if input.TaskID == "" || input.ExpectedRevision < 1 || strings.TrimSpace(input.Text) == "" || len(input.Text) > 4096 {
		return BenchmarkRevisionChatReceipt{}, ErrBenchmarkInput
	}
	key := benchmarkRevisionChatKey(input.RequestID)
	fingerprint := digest(toJSON([]any{actor, conversation, input}))
	prior := func() (BenchmarkRevisionChatReceipt, error) {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return BenchmarkRevisionChatReceipt{}, err
		}
		defer tx.Rollback()
		existing, err := previousSubmission(tx, key, fingerprint)
		if err != nil {
			return BenchmarkRevisionChatReceipt{}, err
		}
		if existing.ID != "" {
			return readBenchmarkRevisionChat(tx, actor, conversation, existing.ID)
		}
		var state, before string
		err = tx.QueryRowContext(ctx, "SELECT state,fingerprint FROM benchmark_revision_requests WHERE request_key=?", key).Scan(&state, &before)
		if err == nil {
			if before != fingerprint {
				return BenchmarkRevisionChatReceipt{}, ErrConflict
			}
			if state == "running" {
				return BenchmarkRevisionChatReceipt{}, ErrActive
			}
			return BenchmarkRevisionChatReceipt{}, ErrBenchmarkRevisionRequestInterrupted
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return BenchmarkRevisionChatReceipt{}, err
		}
		if err := checkConversationSubmission(tx, conversation); err != nil {
			return BenchmarkRevisionChatReceipt{}, err
		}
		var active int
		if err := tx.QueryRowContext(ctx, "SELECT plan_revision FROM benchmark_runs WHERE task_id=? AND actor=?", input.TaskID, actor).Scan(&active); err != nil {
			return BenchmarkRevisionChatReceipt{}, err
		}
		if active != input.ExpectedRevision {
			return BenchmarkRevisionChatReceipt{}, ErrTaskRevision
		}
		stamp := now()
		if _, err := tx.ExecContext(ctx, `INSERT INTO benchmark_revision_requests VALUES(?,?,?,?,?,'running',?,?)`, key, fingerprint, actor, conversation, input.TaskID, stamp, stamp); err != nil {
			return BenchmarkRevisionChatReceipt{}, err
		}
		return BenchmarkRevisionChatReceipt{}, tx.Commit()
	}
	if previous, err := prior(); err != nil || previous.RunID != "" {
		return previous, err
	}
	completed := false
	defer func() {
		if !completed {
			s.interruptBenchmarkRevisionRequestOnExit(ctx, key)
		}
	}()
	shortcut, deterministic, err := benchmarkRevisionChatShortcut(input.Text, input.AllowExpectedActualChange)
	if err != nil {
		return BenchmarkRevisionChatReceipt{}, err
	}
	var proposal BenchmarkRevisionProposal
	if deterministic {
		proposal, err = s.ProposeBenchmarkRevision(ctx, actor, input.TaskID, BenchmarkRevisionProposalInput{
			ExpectedRevision: input.ExpectedRevision, Text: shortcut,
		}, provider)
	} else {
		proposal, err = s.ProposeBenchmarkRevisionAI(ctx, actor, input.TaskID, input.BenchmarkRevisionAIInput, provider, interpret)
	}
	if err != nil {
		return BenchmarkRevisionChatReceipt{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BenchmarkRevisionChatReceipt{}, err
	}
	defer tx.Rollback()
	if existing, err := previousSubmission(tx, key, fingerprint); err != nil || existing.ID != "" {
		if err != nil {
			return BenchmarkRevisionChatReceipt{}, err
		}
		return readBenchmarkRevisionChat(tx, actor, conversation, existing.ID)
	}
	if err := checkConversationSubmission(tx, conversation); err != nil {
		return BenchmarkRevisionChatReceipt{}, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE benchmark_revision_requests SET state='completed',updated_at=?
WHERE request_key=? AND fingerprint=? AND state='running'`, now(), key, fingerprint)
	if err != nil {
		return BenchmarkRevisionChatReceipt{}, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return BenchmarkRevisionChatReceipt{}, err
	} else if n != 1 {
		return BenchmarkRevisionChatReceipt{}, ErrBenchmarkRevisionRequestInterrupted
	}

	// The selected case may cross dispatch even without a plan revision.
	// Recheck both the version and the dispatch boundary before recording.
	var active int
	var phase, operation, taskState string
	var stopping bool
	err = tx.QueryRowContext(ctx, `SELECT b.plan_revision,c.phase,c.operation_id,b.stop_requested,t.state
FROM benchmark_runs b JOIN benchmark_cases c ON c.task_id=b.task_id JOIN task_states t ON t.task_id=b.task_id
WHERE b.task_id=? AND b.actor=? AND c.case_key=?`, input.TaskID, actor, proposal.Interpretation.CaseKey).
		Scan(&active, &phase, &operation, &stopping, &taskState)
	if errors.Is(err, sql.ErrNoRows) || err == nil && active != input.ExpectedRevision {
		return BenchmarkRevisionChatReceipt{}, ErrTaskRevision
	}
	if err != nil {
		return BenchmarkRevisionChatReceipt{}, err
	}
	if stopping || (taskState != "pending" && taskState != "running" && taskState != "paused") || phase != "queued" || operation != "" {
		return BenchmarkRevisionChatReceipt{}, ErrState
	}
	out := BenchmarkRevisionChatReceipt{RunID: id(), ConversationID: conversation, TaskID: input.TaskID, SourceRunID: input.SourceRunID, Proposal: proposal}
	response := fmt.Sprintf("已生成基准任务 %s 的方案修订预演（基于第 %d 版）。尚未提交或执行，请核对差异。", input.TaskID, input.ExpectedRevision)
	stamp := now()
	if _, err := tx.ExecContext(ctx, "INSERT INTO runs(id,conversation_id,status,response,created_at) VALUES(?,?,'completed',?,?)", out.RunID, conversation, response, stamp); err != nil {
		return BenchmarkRevisionChatReceipt{}, err
	}
	messageText := input.Text
	if input.SourceRunID != "" {
		messageText = "查看刚才标注的基准修订预演"
	}
	msg, err := tx.ExecContext(ctx, "INSERT INTO messages(conversation_id,run_id,role,text) VALUES(?,?,'user',?)", conversation, out.RunID, messageText)
	if err != nil {
		return BenchmarkRevisionChatReceipt{}, err
	}
	seq, err := msg.LastInsertId()
	if err != nil {
		return BenchmarkRevisionChatReceipt{}, err
	}
	if err := recordInputEvent(tx, seq, key); err != nil {
		return BenchmarkRevisionChatReceipt{}, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO input_dispositions VALUES(?,?,0,'completed',?)", seq, out.RunID, stamp); err != nil {
		return BenchmarkRevisionChatReceipt{}, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO messages(conversation_id,run_id,role,text) VALUES(?,?,'assistant',?)", conversation, out.RunID, response); err != nil {
		return BenchmarkRevisionChatReceipt{}, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO submissions VALUES(?,?,?)", key, fingerprint, out.RunID); err != nil {
		return BenchmarkRevisionChatReceipt{}, err
	}
	// Live asset impact includes the occupancy snapshot. Keep it out of the
	// durable conversation event and same-key replay payload.
	recorded := out
	recorded.Proposal.Preview.AssetImpact = nil
	if err := addEvent(tx, out.RunID, benchmarkRevisionChatEvent, recorded); err != nil {
		return BenchmarkRevisionChatReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return BenchmarkRevisionChatReceipt{}, err
	}
	completed = true
	return out, nil
}

var ErrBenchmarkRevisionPreviewStale = errors.New("预演已失效，请按当前任务状态重新生成预演")

// ReopenBenchmarkRevisionChat reads only the original server-produced intent.
// It asks neither a client nor the model to supply the old plan/difference.
// Fresh asset detail is computed by the ordinary preview and never re-saved.
func (s *Store) ReopenBenchmarkRevisionChat(ctx context.Context, actor, conversation, run string, provider BenchmarkProvider) (BenchmarkRevisionChatReceipt, error) {
	recorded, err := checkedRecordedBenchmarkPreview(s.db, actor, conversation, run)
	if err != nil {
		return BenchmarkRevisionChatReceipt{}, err
	}
	p := recorded.Proposal
	current, err := s.benchmarkRevisionProposalBase(ctx, actor, recorded.TaskID, p.ExpectedRevision)
	if errors.Is(err, ErrTaskRevision) {
		return BenchmarkRevisionChatReceipt{}, ErrBenchmarkRevisionPreviewStale
	}
	if err != nil {
		return BenchmarkRevisionChatReceipt{}, err
	}
	fresh, err := s.previewBenchmarkRevisionIntent(ctx, actor, recorded.TaskID, current, p.Interpretation, provider, p.Method)
	switch {
	case errors.Is(err, ErrAuthorizationDenied):
		return BenchmarkRevisionChatReceipt{}, ErrAuthorizationDenied
	case errors.Is(err, ErrTaskRevision), errors.Is(err, ErrConflict), errors.Is(err, ErrState),
		errors.Is(err, ErrBenchmarkInput), errors.Is(err, ErrBenchmarkBinding):
		return BenchmarkRevisionChatReceipt{}, ErrBenchmarkRevisionPreviewStale
	case err != nil:
		return BenchmarkRevisionChatReceipt{}, err
	}
	// A new backend interpretation of an old stored intent must not silently
	// generate a different proposal while offering the old card again.
	if toJSON(fresh.Preview.Profile) != toJSON(p.Preview.Profile) {
		return BenchmarkRevisionChatReceipt{}, ErrBenchmarkRevisionPreviewStale
	}
	recorded.Proposal = fresh
	return recorded, nil
}

func checkedRecordedBenchmarkPreview(q recordsQuerier, actor, conversation, run string) (BenchmarkRevisionChatReceipt, error) {
	if actor == "" {
		return BenchmarkRevisionChatReceipt{}, sql.ErrNoRows
	}
	recorded, err := readBenchmarkRevisionChat(q, actor, conversation, run)
	if err != nil {
		return BenchmarkRevisionChatReceipt{}, err
	}
	p := recorded.Proposal
	if recorded.RunID != run || recorded.ConversationID != conversation || p.State != "preview_only" ||
		(p.Method != "model_interpreted" && p.Method != "explicit_shortcut") || p.ExpectedRevision < 1 || p.Preview.Revision != p.ExpectedRevision+1 ||
		(p.Interpretation.Action != "remove_case" && p.Interpretation.Action != "set_parameter") {
		return BenchmarkRevisionChatReceipt{}, ErrBenchmarkRevisionPreviewStale
	}
	return recorded, nil
}

const benchmarkChatConfirmationPrefix = "chat-preview-confirm:"

type benchmarkRevisionConfirmation struct {
	conversation string
	run          string
	expected     int
}

type BenchmarkRevisionChatConfirmInput struct {
	Action           string `json:"action"`
	ExpectedRevision int    `json:"expected_revision"`
}

type BenchmarkRevisionChatConfirmation struct {
	RunID    string `json:"preview_run_id"`
	Revision int    `json:"revision"`
	State    string `json:"state"`
}

// ConfirmBenchmarkRevisionChat applies precisely a server-stored preview,
// rechecking authority and write preconditions in the same transaction.
// Repeated confirmations return only their original revision receipt.
func (s *Store) ConfirmBenchmarkRevisionChat(ctx context.Context, actor, conversation, run string, input BenchmarkRevisionChatConfirmInput, provider BenchmarkProvider) (BenchmarkRevisionChatConfirmation, error) {
	if input.Action != "apply_benchmark_revision" || input.ExpectedRevision < 1 || conversation == "" || run == "" {
		return BenchmarkRevisionChatConfirmation{}, ErrBenchmarkInput
	}
	applied, err := s.reviseBenchmark(ctx, actor, "", BenchmarkRevisionInput{}, provider, true,
		&benchmarkRevisionConfirmation{conversation: conversation, run: run, expected: input.ExpectedRevision})
	switch {
	case errors.Is(err, ErrAuthorizationDenied):
		return BenchmarkRevisionChatConfirmation{}, ErrAuthorizationDenied
	case errors.Is(err, ErrTaskRevision), errors.Is(err, ErrConflict), errors.Is(err, ErrState),
		errors.Is(err, ErrBenchmarkInput), errors.Is(err, ErrBenchmarkBinding):
		return BenchmarkRevisionChatConfirmation{}, ErrBenchmarkRevisionPreviewStale
	case err != nil:
		return BenchmarkRevisionChatConfirmation{}, err
	}
	return BenchmarkRevisionChatConfirmation{RunID: run, Revision: applied.Revision, State: "applied"}, nil
}
