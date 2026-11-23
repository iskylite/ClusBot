package work

import "context"

func (s *Store) ReadTaskSummary(ctx context.Context, id string) (Task, error) {
	return scanTask(s.db.QueryRowContext(ctx, "SELECT "+taskColumns+" FROM tasks WHERE id=?", id))
}

func (s *Store) AdminTaskStatusCounts(ctx context.Context, filter TaskFilter) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT status, count(*) FROM tasks WHERE "+taskSearch+" GROUP BY status",
		taskSearchArgs(filter.Scope, filter.Query, filter.MatchID)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := make(map[string]int)
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		counts[status] = count
	}
	return counts, rows.Err()
}

type AdminTaskExecutionSummary struct {
	RunCount      int    `json:"run_count"`
	ArtifactCount int    `json:"artifact_count"`
	LatestRunID   string `json:"latest_run_id,omitempty"`
}

// AdminTaskExecutionSummaries reads list metadata without reading private run
// inputs, responses or artifact content.
func (s *Store) AdminTaskExecutionSummaries(ctx context.Context, ids []string) (map[string]AdminTaskExecutionSummary, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.id,
 (SELECT count(*) FROM runs r WHERE r.task_id=t.id),
 (SELECT count(*) FROM artifacts a WHERE a.task_id=t.id),
 coalesce((SELECT r.id FROM runs r WHERE r.task_id=t.id ORDER BY r.rowid DESC LIMIT 1),'')
 FROM tasks t WHERE t.id IN (SELECT value FROM json_each(?))`, toJSON(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make(map[string]AdminTaskExecutionSummary, len(ids))
	for rows.Next() {
		var id string
		var summary AdminTaskExecutionSummary
		if err := rows.Scan(&id, &summary.RunCount, &summary.ArtifactCount, &summary.LatestRunID); err != nil {
			return nil, err
		}
		items[id] = summary
	}
	return items, rows.Err()
}

// TaskExecutionState contains operational fields only. Private input, model
// context, tool evidence and free-form error text stay in the personal API.
type TaskExecutionState struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	CreatedAt  int64  `json:"created_at"`
	Generation int    `json:"execution_generation"`
	ErrorCode  string `json:"error_code,omitempty"`
}

type TaskExecutionPage struct {
	Items []TaskExecutionState `json:"items"`
	Next  string               `json:"next_cursor,omitempty"`
}

func (s *Store) ListTaskExecutionStates(ctx context.Context, task, after string) (TaskExecutionPage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,status,created_at,execution_generation,coalesce(error_code,'')
FROM runs WHERE task_id=? AND (?='' OR rowid<(SELECT rowid FROM runs WHERE task_id=? AND id=?))
ORDER BY rowid DESC LIMIT ?`, task, after, task, after, runPageSize+1)
	if err != nil {
		return TaskExecutionPage{}, err
	}
	defer rows.Close()
	items := make([]TaskExecutionState, 0, runPageSize)
	for rows.Next() {
		var run TaskExecutionState
		if err := rows.Scan(&run.ID, &run.Status, &run.CreatedAt, &run.Generation, &run.ErrorCode); err != nil {
			return TaskExecutionPage{}, err
		}
		items = append(items, run)
	}
	if err := rows.Err(); err != nil {
		return TaskExecutionPage{}, err
	}
	page := TaskExecutionPage{Items: items}
	if len(items) > runPageSize {
		page.Items = items[:runPageSize]
		page.Next = page.Items[runPageSize-1].ID
	}
	return page, nil
}
