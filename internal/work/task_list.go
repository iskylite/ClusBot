package work

import (
	"context"
	"database/sql"
	"strconv"
)

const TaskPageLimit = 200

type TaskFilter struct {
	Scope, Query, Status string
	AfterUpdatedAt       int64
	AfterID              string
	Limit                int
}

const taskSearch = `(?='' OR scope_label=?) AND (?='' OR instr(lower(title),lower(?))>0 OR instr(lower(goal),lower(?))>0)`
const taskUpdatedOrder = "updated_at DESC,id"

type taskListQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func queryTasks(ctx context.Context, q taskListQueryer, filter TaskFilter, where, order string, args ...any) ([]Task, error) {
	params := taskSearchArgs(filter.Scope, filter.Query)
	params = append(params, args...)
	params = append(params, filter.Limit)
	rows, err := q.QueryContext(ctx, "SELECT "+taskColumns+" FROM tasks WHERE "+taskSearch+" AND ("+where+") ORDER BY "+order+" LIMIT ?", params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Task{}
	for rows.Next() {
		item, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func taskSearchArgs(scope, query string) []any {
	return []any{scope, scope, query, query, query}
}

func taskPageLimit(limit int) int {
	if limit < 1 || limit > TaskPageLimit {
		return TaskPageLimit
	}
	return limit
}

func (s *Store) ListTasks(ctx context.Context, filter TaskFilter) ([]Task, error) {
	filter.Limit = taskPageLimit(filter.Limit)
	return queryTasks(ctx, s.db, filter, `(?='' OR status=?) AND (?='' OR updated_at<? OR (updated_at=? AND id>?))`, taskUpdatedOrder,
		filter.Status, filter.Status, filter.AfterID, filter.AfterUpdatedAt, filter.AfterUpdatedAt, filter.AfterID)
}

const requiresTaskDecision = `(requirements_changed=1
OR EXISTS(SELECT 1 FROM runs r JOIN run_reads d ON d.run_id=r.id WHERE r.task_id=tasks.id AND d.status='unknown' AND d.acknowledged_at=0)
OR EXISTS(SELECT 1 FROM publications p WHERE p.task_id=tasks.id AND p.status='unknown'))`

const taskBackgroundWait = "status_reason='file_writing'"

type TaskCursor struct {
	Timestamp int64
	ID        string
}

type OverviewFilter struct {
	Scope, Query string
	Limit        int
	After        map[string]TaskCursor
}

type OverviewSnapshot struct {
	Groups      map[string][]Task `json:"groups"`
	Counts      map[string]int    `json:"counts"`
	NextCursors map[string]string `json:"next_cursors"`
	Scopes      []string          `json:"scopes"`
	SnapshotAt  int64             `json:"snapshot_at"`
}

var overviewGroups = []struct {
	name, where, timestamp string
	ascending              bool
	cursorTime             func(Task) int64
}{
	{"waiting", "(status='waiting_user' AND NOT " + taskBackgroundWait + ") OR (status='interrupted' AND " + requiresTaskDecision + ")",
		"state_changed_at", true, func(task Task) int64 { return task.StateChangedAt }},
	{"active", "status='active' OR (status='waiting_user' AND " + taskBackgroundWait + ") OR (status='interrupted' AND NOT " + requiresTaskDecision + ")",
		"updated_at", false, func(task Task) int64 { return task.UpdatedAt }},
	{"completed", "status='completed'", "coalesce(completed_at,updated_at)", false, func(task Task) int64 {
		if task.CompletedAt == nil {
			return task.UpdatedAt
		}
		return *task.CompletedAt
	}},
}

func (s *Store) Overview(ctx context.Context, filter OverviewFilter) (OverviewSnapshot, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OverviewSnapshot{}, err
	}
	defer tx.Rollback()
	pageSize := taskPageLimit(filter.Limit)
	listFilter := TaskFilter{Scope: filter.Scope, Query: filter.Query, Limit: pageSize + 1}
	snapshot := OverviewSnapshot{
		Groups: make(map[string][]Task, len(overviewGroups)), Counts: make(map[string]int, len(overviewGroups)),
		NextCursors: make(map[string]string), Scopes: []string{}, SnapshotAt: now(),
	}
	for _, group := range overviewGroups {
		var count int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM tasks WHERE "+taskSearch+" AND ("+group.where+")",
			taskSearchArgs(filter.Scope, filter.Query)...).Scan(&count); err != nil {
			return OverviewSnapshot{}, err
		}
		snapshot.Counts[group.name] = count
		where := "(" + group.where + ")"
		direction, comparison := " DESC", "<"
		if group.ascending {
			direction, comparison = " ASC", ">"
		}
		var args []any
		if cursor := filter.After[group.name]; cursor.ID != "" {
			where += " AND (" + group.timestamp + comparison + "? OR (" + group.timestamp + "=? AND id>?))"
			args = []any{cursor.Timestamp, cursor.Timestamp, cursor.ID}
		}
		items, err := queryTasks(ctx, tx, listFilter, where, group.timestamp+direction+",id", args...)
		if err != nil {
			return OverviewSnapshot{}, err
		}
		if len(items) > pageSize {
			items = items[:pageSize]
			last := items[pageSize-1]
			snapshot.NextCursors[group.name] = strconv.FormatInt(group.cursorTime(last), 10) + ":" + last.ID
		}
		snapshot.Groups[group.name] = items
	}
	rows, err := tx.QueryContext(ctx, "SELECT DISTINCT scope_label FROM tasks WHERE scope_label<>'' ORDER BY scope_label")
	if err != nil {
		return OverviewSnapshot{}, err
	}
	for rows.Next() {
		var scope string
		if err = rows.Scan(&scope); err != nil {
			rows.Close()
			return OverviewSnapshot{}, err
		}
		snapshot.Scopes = append(snapshot.Scopes, scope)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return OverviewSnapshot{}, err
	}
	return snapshot, tx.Commit()
}
