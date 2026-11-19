package work

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var ErrBenchmarkRevisionRequestInterrupted = errors.New("上次预演没有完成，不会自动重发模型请求；请用新请求重新预演")

func migrateBenchmarkRevisionRequests(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS benchmark_revision_requests (
 request_key TEXT PRIMARY KEY, fingerprint TEXT NOT NULL, actor TEXT NOT NULL,
 conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
 task_id TEXT NOT NULL REFERENCES benchmark_runs(task_id) ON DELETE CASCADE,
 state TEXT NOT NULL CHECK(state IN ('running','completed','interrupted')),
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS benchmark_revision_running ON benchmark_revision_requests(state) WHERE state='running'`)
	return err
}

func benchmarkRevisionChatKey(requestID string) string {
	return "benchmark-revision-preview:" + digest(requestID)
}

type BenchmarkRevisionRequestState struct {
	State      string `json:"state"`
	RunID      string `json:"preview_run_id,omitempty"`
	CanRestart bool   `json:"can_start_new_request"`
}

func (s *Store) ReadBenchmarkRevisionRequestState(ctx context.Context, actor, conversation, requestID string) (BenchmarkRevisionRequestState, error) {
	key := benchmarkRevisionChatKey(requestID)
	var out BenchmarkRevisionRequestState
	err := s.db.QueryRowContext(ctx, `SELECT q.state,CASE WHEN q.state='completed' THEN coalesce(s.run_id,'') ELSE '' END
FROM benchmark_revision_requests q JOIN benchmark_runs b ON b.task_id=q.task_id AND b.actor=q.actor
LEFT JOIN submissions s ON s.request_key=q.request_key
WHERE q.request_key=? AND q.actor=? AND q.conversation_id=?`, key, actor, conversation).Scan(&out.State, &out.RunID)
	if errors.Is(err, sql.ErrNoRows) {
		// Receipts persisted before the request ledger was introduced.
		var run string
		submissionErr := s.db.QueryRowContext(ctx, "SELECT run_id FROM submissions WHERE request_key=?", key).Scan(&run)
		if submissionErr != nil {
			return BenchmarkRevisionRequestState{}, submissionErr
		}
		if _, readErr := readBenchmarkRevisionChat(s.db, actor, conversation, run); readErr != nil {
			return BenchmarkRevisionRequestState{}, readErr
		}
		return BenchmarkRevisionRequestState{State: "completed", RunID: run}, nil
	}
	if err != nil {
		return BenchmarkRevisionRequestState{}, err
	}
	out.CanRestart = out.State == "interrupted"
	return out, nil
}

// Interrupted model requests have a durable boundary but no trusted result.
// Never automatically reclaim them for the same idempotency key.
func (s *Store) interruptBenchmarkRevisionRequests(ctx context.Context, key string, stamp int64) (int, error) {
	query := "UPDATE benchmark_revision_requests SET state='interrupted',updated_at=? WHERE state='running'"
	args := []any{stamp}
	if key != "" {
		query += " AND request_key=?"
		args = append(args, key)
	}
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func (s *Store) interruptBenchmarkRevisionRequestOnExit(ctx context.Context, key string) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	// If persistence is unavailable, startup recovery handles the retained claim.
	_, _ = s.interruptBenchmarkRevisionRequests(cleanup, key, now())
}
