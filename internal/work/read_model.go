package work

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrReadModelCall    = errors.New("模型未发出有效的读取调用")
	errReadOutputBudget = errors.New("tool round trip needs at least 32 output tokens")
)

type TaskReadCall struct {
	Protocol string
	CallID   string
	Output   json.RawMessage
}

type TaskReadModel interface {
	PlanTaskRead(context.Context, []Message, int) (TaskReadCall, *TokenUsage, error)
	CompleteTaskRead(context.Context, []Message, TaskReadCall, *TaskRead, int, func(string) error) (TaskAnswer, error)
}

type taskReadProvider interface {
	TaskReadModel(context.Context) (TaskReadModel, error)
}

func (s *Store) saveReadModelCall(ctx context.Context, runID string, call TaskReadCall, usage *TokenUsage) error {
	if call.Protocol == "" || call.CallID == "" || len(call.Output) > 2<<20 || !json.Valid(call.Output) {
		return ErrReadModelCall
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO run_read_model_calls(run_id,protocol,call_id,output)
SELECT d.run_id,?,?,? FROM run_reads d JOIN runs r ON r.id=d.run_id
WHERE d.run_id=? AND d.position=1 AND d.status='queued' AND r.status='running' AND r.control='' AND r.model_requests=r.admission_requests+1`,
		call.Protocol, call.CallID, string(call.Output), runID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrState
	}
	if err = recordModelUsage(tx, runID, usage); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) prepareReadFollowup(ctx context.Context, runID string, elapsed time.Duration) (TaskReadCall, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TaskReadCall{}, err
	}
	defer tx.Rollback()
	var call TaskReadCall
	var output string
	err = tx.QueryRowContext(ctx, `SELECT m.protocol,m.call_id,m.output FROM run_read_model_calls m
JOIN run_reads d ON d.run_id=m.run_id AND d.position=1 WHERE m.run_id=? AND d.status='completed'`, runID).
		Scan(&call.Protocol, &call.CallID, &output)
	if err != nil {
		return TaskReadCall{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE runs SET model_requests=model_requests+1,active_ms=max(active_ms,?)
WHERE id=? AND status='running' AND control='' AND model_requests=admission_requests+1`, elapsed.Milliseconds(), runID)
	if err != nil {
		return TaskReadCall{}, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return TaskReadCall{}, err
	}
	if n != 1 {
		return TaskReadCall{}, ErrState
	}
	call.Output = json.RawMessage(output)
	return call, tx.Commit()
}

func (s *Store) streamWithTaskRead(ctx context.Context, runID string, started time.Time, model TaskReadModel, input []Message, limits RunLimits, emit func(context.Context, string) error) (TaskAnswer, error) {
	// Reserve part of the Run's output budget for the answer even if the
	// provider does not report the tool request's token usage.
	planLimit := min(256, limits.OutputTokens/2)
	modelCtx, cancel := context.WithTimeoutCause(ctx, limits.ModelTimeout, errModelTimeout)
	call, usage, err := model.PlanTaskRead(modelCtx, input, planLimit)
	if cause := context.Cause(modelCtx); cause != nil {
		err = cause
	}
	cancel()
	if err != nil {
		return TaskAnswer{Usage: usage}, err
	}
	if usage != nil && usage.OutputTokens != nil && *usage.OutputTokens > int64(planLimit) {
		return TaskAnswer{Usage: usage}, ErrOutputBudget
	}
	if err = s.saveReadModelCall(ctx, runID, call, usage); err != nil {
		return TaskAnswer{Usage: usage}, err
	}
	read, err := s.runTaskRead(ctx, runID)
	if err != nil {
		return TaskAnswer{}, err
	}
	call, err = s.prepareReadFollowup(ctx, runID, time.Since(started))
	if err != nil {
		return TaskAnswer{}, err
	}
	if err = s.checkTaskReadModelAuthority(ctx, runID); err != nil {
		return TaskAnswer{}, err
	}
	modelCtx, cancel = context.WithTimeoutCause(ctx, limits.ModelTimeout, errModelTimeout)
	defer cancel()
	answerLimit := limits.OutputTokens - planLimit
	answer, err := model.CompleteTaskRead(modelCtx, input, call, read, answerLimit, func(text string) error {
		return emit(modelCtx, text)
	})
	if cause := context.Cause(modelCtx); cause != nil {
		err = cause
	}
	if err == nil && answer.Usage != nil && answer.Usage.OutputTokens != nil && *answer.Usage.OutputTokens > int64(answerLimit) {
		err = ErrOutputBudget
	}
	return answer, err
}
