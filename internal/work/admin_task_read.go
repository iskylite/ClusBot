package work

import "context"

func (s *Store) ReadTaskSummary(ctx context.Context, id string) (Task, error) {
	return scanTask(s.db.QueryRowContext(ctx, "SELECT "+taskColumns+" FROM tasks WHERE id=?", id))
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
