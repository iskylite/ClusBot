package work

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

var ErrTaskRevision = errors.New("事项已变更，请载入最新版本后重试")

type TaskEditError string

func (e TaskEditError) Error() string { return string(e) }

type TaskEdit struct {
	Revision     int     `json:"revision"`
	Title        *string `json:"title"`
	Goal         *string `json:"goal"`
	Scope        *string `json:"scope_label"`
	Criteria     *string `json:"acceptance_criteria"`
	RequiredFile *string `json:"required_filename"`
}

func (s *Store) EditTask(ctx context.Context, taskID string, edit TaskEdit) (Task, error) {
	if edit.Revision < 1 || (edit.Title == nil && edit.Goal == nil && edit.Scope == nil && edit.Criteria == nil && edit.RequiredFile == nil) {
		return Task{}, TaskEditError("需要事项版本和至少一项修改")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback()
	saved, err := editTaskTx(ctx, tx, taskID, edit)
	if err != nil {
		return Task{}, err
	}
	return saved, tx.Commit()
}

func editTaskTx(ctx context.Context, tx *sql.Tx, taskID string, edit TaskEdit) (Task, error) {
	task, err := scanTask(tx.QueryRowContext(ctx, "SELECT "+taskColumns+" FROM tasks WHERE id=?", taskID))
	if err != nil {
		return Task{}, err
	}
	var benchmark bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM benchmark_runs WHERE task_id=?)", taskID).Scan(&benchmark); err != nil {
		return Task{}, err
	}
	if benchmark {
		return Task{}, fmt.Errorf("%w：基准任务须使用基准方案修订接口", ErrState)
	}
	if task.Revision != edit.Revision {
		return Task{}, ErrTaskRevision
	}
	if err = conversationIdle(tx, task.ConversationID); err != nil {
		return Task{}, err
	}
	next := task
	for _, field := range []struct {
		value *string
		dest  *string
		min   int
		max   int
		error TaskEditError
	}{
		{edit.Title, &next.Title, 1, 60, "请输入 1 至 60 字的事项标题"},
		{edit.Goal, &next.Goal, 1, 16000, "请输入 1 至 16000 字的事项目标"},
		{edit.Scope, &next.Scope, 0, 40, "工作范围最多 40 字"},
		{edit.Criteria, &next.Criteria, 0, 1000, "验收条件最多 1000 字"},
	} {
		if field.value == nil {
			continue
		}
		value := strings.TrimSpace(*field.value)
		if length := utf8.RuneCountInString(value); length < field.min || length > field.max {
			return Task{}, field.error
		}
		*field.dest = value
	}
	if edit.RequiredFile != nil {
		next.RequiredFile = strings.TrimSpace(*edit.RequiredFile)
		if next.RequiredFile != "" && !validFilename(next.RequiredFile) {
			return Task{}, ErrFilename
		}
	}
	if next == task {
		return task, nil
	}
	newRequirements := next.Goal != task.Goal || next.Criteria != task.Criteria || next.RequiredFile != task.RequiredFile
	if newRequirements {
		next.RequirementsChanged = true
		if task.Status != "cancelled" {
			next.Status, next.StatusReason, next.Reason = "interrupted", "requirements_changed", "工作要求已修改，等待按新要求执行"
		}
	}
	next.Revision++
	next.UpdatedAt = now()
	_, err = tx.ExecContext(ctx, `UPDATE tasks SET title=?,goal=?,scope_label=?,acceptance_criteria=?,required_filename=?,
status=?,status_reason=?,reason=?,requirements_changed=?,completed_at=CASE WHEN ? THEN NULL ELSE completed_at END,revision=?,updated_at=? WHERE id=?`,
		next.Title, next.Goal, next.Scope, next.Criteria, next.RequiredFile, next.Status, next.StatusReason, next.Reason, next.RequirementsChanged,
		newRequirements, next.Revision, next.UpdatedAt, taskID)
	if err != nil {
		return Task{}, err
	}
	return scanTask(tx.QueryRowContext(ctx, "SELECT "+taskColumns+" FROM tasks WHERE id=?", taskID))
}
