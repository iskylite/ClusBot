package work

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var (
	ErrOutputBudget       = errors.New("model output budget exhausted")
	ErrModelRequestBudget = errors.New("model request exceeds input budget")
	errRunBudget          = errors.New("run time budget exhausted")
	errModelTimeout       = errors.New("model request timed out")
)

type RunLimits struct {
	ActiveTime   time.Duration
	ModelTimeout time.Duration
	OutputTokens int
}

type TokenUsage struct {
	InputTokens  *int64 `json:"input_tokens"`
	OutputTokens *int64 `json:"output_tokens"`
}

type ExecutionUsage struct {
	TokenUsage
	// LastInputTokens is the input size of the most recent model request in a
	// single run, used to show context usage against the window. It is omitted
	// from task-level aggregation, where a single "last request" has no meaning.
	LastInputTokens    *int64 `json:"last_input_tokens,omitempty"`
	ModelRequests      int    `json:"model_requests"`
	UnreportedRequests int    `json:"unreported_requests"`
	ActiveMS           int64  `json:"active_ms"`
	TimePartial        bool   `json:"time_partial"`
	UntrackedRuns      int    `json:"untracked_runs"`
}

type runAccounting struct {
	tokens    *TokenUsage
	question  string
	plan      string
	activeMS  int64
	errorCode string
}

func taskExecutionUsage(tx *sql.Tx, id string) (ExecutionUsage, error) {
	var usage ExecutionUsage
	err := tx.QueryRow(`SELECT coalesce(sum(model_requests),0),
coalesce(sum(model_requests-reported_requests),0),
coalesce(sum(active_ms),0), coalesce(max(time_partial),0),
coalesce(sum(CASE WHEN usage_tracked=0 AND status<>'queued' THEN 1 ELSE 0 END),0),
sum(input_tokens), sum(output_tokens)
FROM runs WHERE task_id=?`, id).Scan(&usage.ModelRequests, &usage.UnreportedRequests,
		&usage.ActiveMS, &usage.TimePartial, &usage.UntrackedRuns, &usage.InputTokens, &usage.OutputTokens)
	return usage, err
}

func recordModelUsage(tx *sql.Tx, runID string, tokens *TokenUsage) error {
	if tokens == nil {
		return nil
	}
	reported := 0
	if tokens.InputTokens != nil && tokens.OutputTokens != nil {
		reported = 1
	}
	_, err := tx.Exec(`UPDATE runs SET input_tokens=coalesce(input_tokens+?,input_tokens,?),
output_tokens=coalesce(output_tokens+?,output_tokens,?),reported_requests=reported_requests+?,
last_input_tokens=coalesce(?,last_input_tokens) WHERE id=?`,
		tokens.InputTokens, tokens.InputTokens, tokens.OutputTokens, tokens.OutputTokens, reported, tokens.InputTokens, runID)
	return err
}

func (s *Store) prepareModelRequest(ctx context.Context, runID string, elapsed time.Duration, query string) ([]Memory, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE runs SET model_requests=model_requests+1,active_ms=max(active_ms,?)
WHERE id=? AND status='running' AND control='' AND model_requests=admission_requests`, elapsed.Milliseconds(), runID)
	if err != nil {
		return nil, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if changed != 1 {
		return nil, ErrState
	}
	items, err := listMemories(ctx, tx)
	if err != nil {
		return nil, err
	}
	selected := matchingMemories(items, query)
	for _, m := range selected {
		if _, err = tx.ExecContext(ctx, "INSERT INTO run_memories(run_id,memory_id,revision,title) VALUES(?,?,?,?)", runID, m.ID, m.Revision, m.Title); err != nil {
			return nil, err
		}
	}
	return selected, tx.Commit()
}
