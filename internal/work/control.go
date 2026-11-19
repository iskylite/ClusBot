package work

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Control records an intent before signalling a running worker. Queued work can
// transition immediately because it has not reached the provider.
func (s *Store) Control(ctx context.Context, taskID, action string, revision int) error {
	if action != "pause" && action != "stop" {
		return ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := controlTask(tx, taskID, action, revision); err != nil {
		return err
	}
	return tx.Commit()
}

func controlTask(tx *sql.Tx, taskID, action string, revision int) error {
	task, err := scanTask(tx.QueryRow("SELECT "+taskColumns+" FROM tasks WHERE id=?", taskID))
	if err != nil {
		return err
	}
	if err = ensureNoPublication(tx, taskID); err != nil {
		return err
	}
	if action == "stop" {
		if err = ensureReadConfirmed(tx, taskID); err != nil {
			return err
		}
	}
	if task.Revision != revision || task.Status == "completed" || task.Status == "cancelled" || (action == "pause" && task.Status != "active") {
		return ErrState
	}
	if handled, err := controlBenchmark(tx, taskID, action); handled || err != nil {
		return err
	}

	var runID, status, pending string
	err = tx.QueryRow("SELECT id,status,control FROM runs WHERE task_id=? ORDER BY rowid DESC LIMIT 1", taskID).Scan(&runID, &status, &pending)
	if err != nil {
		return err
	}
	if pending != "" && status == "running" {
		return ErrState
	}
	runStatus, taskStatus, code, reason := controlOutcome(action)
	if status == "running" {
		if _, err = tx.Exec("UPDATE runs SET control=? WHERE id=?", action, runID); err != nil {
			return err
		}
		taskStatus = "active"
		if action == "pause" {
			code, reason = "pause_requested", "正在暂停"
		} else {
			code, reason = "stop_requested", "正在停止"
		}
	} else if status == "queued" || (status == "interrupted" && action == "stop") {
		if _, err = tx.Exec("UPDATE runs SET status=?,error=?,error_code=?,control=?,usage_tracked=1 WHERE id=?", runStatus, reason, action, action, runID); err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE run_reads SET status='not_run',detail=? WHERE run_id=? AND status='queued'
AND (?='stop' OR NOT EXISTS(SELECT 1 FROM run_read_checkpoints WHERE run_id=run_reads.run_id))`, reason, runID, action); err != nil {
			return err
		}
		if err = addEvent(tx, runID, "run.state_changed", map[string]string{"status": runStatus, "error": reason}); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("UPDATE tasks SET status=?,status_reason=?,reason=?,revision=revision+1,updated_at=? WHERE id=?", taskStatus, code, reason, now(), taskID); err != nil {
		return err
	}
	if action == "stop" {
		if _, err := tx.Exec("UPDATE task_followups SET cancelled_at=? WHERE task_id=? AND dispatched_run_id IS NULL AND cancelled_at IS NULL", now(), taskID); err != nil {
			return err
		}
		if status != "running" {
			return cancelConversationInputs(tx, task.ConversationID, runID)
		}
	}
	return nil
}

func controlOutcome(action string) (runStatus, taskStatus, code, reason string) {
	if action == "pause" {
		return "interrupted", "interrupted", "paused", "已暂停"
	}
	return "cancelled", "cancelled", "stopped", "已停止"
}

func (s *Store) Resume(ctx context.Context, taskID, requestKey string, revision int) (Run, error) {
	return s.resumeTask(ctx, taskID, requestKey, revision, "resume")
}

func (s *Store) RetryTask(ctx context.Context, taskID, requestKey string, revision int) (Run, error) {
	return s.resumeTask(ctx, taskID, requestKey, revision, "retry")
}

func (s *Store) ReassignTask(ctx context.Context, taskID, requestKey string, revision int) (Run, error) {
	return s.resumeTask(ctx, taskID, requestKey, revision, "reassign")
}

func (s *Store) resumeTask(ctx context.Context, taskID, requestKey string, revision int, action string) (Run, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	run, err := resumeTaskTx(tx, taskID, requestKey, revision, action)
	if err != nil {
		return Run{}, err
	}
	return run, tx.Commit()
}

var ErrBenchmarkRecoveryUnsupported = errors.New("基准任务不能用普通助手继续、重试或重新分派；请查看原作业对账，或另行提交新的基准任务")

var ErrDiscoveryRecoveryUnsupported = errors.New("定时资产发现不能用普通助手继续、重试或重新分派；请检查发现计划和来源授权，等待下一次有效扫描")

func discoveryOccurrenceTask(q recordsQuerier, taskID string) (bool, error) {
	var exists bool
	err := q.QueryRow("SELECT EXISTS(SELECT 1 FROM discovery_occurrences WHERE task_id=?)", taskID).Scan(&exists)
	return exists, err
}

func requireNotDiscoveryRecovery(q recordsQuerier, taskID string) error {
	discovery, err := discoveryOccurrenceTask(q, taskID)
	if err != nil {
		return err
	}
	if discovery {
		return ErrDiscoveryRecoveryUnsupported
	}
	return nil
}

var ErrDeterministicTaskRecoveryUnsupported = errors.New("资产验收查询和工单由专用运行器处理，不能用普通助手继续、重试或重新分派；请查看原查询或工单进度")

// Dedicated query and ticket producers create their own tasks; neither uses
// a personal-worker Run. Match only those persisted producer memberships.
func deterministicDomainTask(q recordsQuerier, taskID string) (bool, error) {
	var exists bool
	err := q.QueryRow(`SELECT EXISTS(SELECT 1 FROM domain_query_tasks WHERE task_id=?)
OR EXISTS(SELECT 1 FROM maintenance_tasks WHERE task_id=?)`, taskID, taskID).Scan(&exists)
	return exists, err
}

// CheckDeterministicFollowupConversation rejects explicit task writes before
// admission (including model-off installations). Submission repeats the guard.
func (s *Store) CheckDeterministicFollowupConversation(ctx context.Context, conversationID string) error {
	if conversationID == "" {
		return nil
	}
	var taskID string
	err := s.db.QueryRowContext(ctx, "SELECT id FROM tasks WHERE conversation_id=?", conversationID).Scan(&taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	deterministic, err := deterministicDomainTask(s.db, taskID)
	if err == nil && deterministic {
		return ErrDeterministicTaskRecoveryUnsupported
	}
	return err
}

func requireOrdinaryTaskFollowup(q recordsQuerier, taskID string) error {
	if err := requireNotDiscoveryRecovery(q, taskID); err != nil {
		return err
	}
	deterministic, err := deterministicDomainTask(q, taskID)
	if err != nil {
		return err
	}
	if deterministic {
		return ErrDeterministicTaskRecoveryUnsupported
	}
	return nil
}

func requireGenericTaskRecovery(q recordsQuerier, taskID string) error {
	var benchmark bool
	if err := q.QueryRow("SELECT EXISTS(SELECT 1 FROM benchmark_runs WHERE task_id=?)", taskID).Scan(&benchmark); err != nil {
		return err
	}
	if benchmark {
		return ErrBenchmarkRecoveryUnsupported
	}
	return requireOrdinaryTaskFollowup(q, taskID)
}

// CheckGenericTaskRecovery lets the HTTP entry point explain the domain guard
// even when the personal model is unavailable. The write path checks again.
func (s *Store) CheckGenericTaskRecovery(taskID string) error {
	return requireGenericTaskRecovery(s.db, taskID)
}

func resumeTaskTx(tx *sql.Tx, taskID, requestKey string, revision int, action string) (Run, error) {
	if err := requireGenericTaskRecovery(tx, taskID); err != nil {
		return Run{}, err
	}
	retry := action == "retry"
	fingerprint := digest(action + ":" + taskID)
	var previous, previousRun string
	err := tx.QueryRow("SELECT fingerprint,run_id FROM submissions WHERE request_key=?", requestKey).Scan(&previous, &previousRun)
	if err == nil {
		if previous != fingerprint {
			return Run{}, ErrConflict
		}
		return scanRun(tx.QueryRow("SELECT "+runColumns+" FROM runs WHERE id=?", previousRun))
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Run{}, err
	}
	if err = ensureGoalTaskContinuable(tx, taskID); err != nil {
		return Run{}, err
	}
	task, err := scanTask(tx.QueryRow("SELECT "+taskColumns+" FROM tasks WHERE id=?", taskID))
	if err != nil {
		return Run{}, err
	}
	if err = ensureNoPublication(tx, taskID); err != nil {
		return Run{}, err
	}
	if err = ensureReadConfirmed(tx, taskID); err != nil {
		return Run{}, err
	}
	if task.Revision != revision || (task.Status != "interrupted" && task.Status != "cancelled") {
		return Run{}, ErrState
	}
	if retry && (task.Status != "interrupted" || task.RequirementsChanged) {
		return Run{}, ErrState
	}
	var active bool
	if err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM runs WHERE task_id=? AND status IN ('queued','running'))", taskID).Scan(&active); err != nil {
		return Run{}, err
	}
	if active {
		return Run{}, ErrActive
	}
	if action == "reassign" {
		if task.Status != "interrupted" {
			return Run{}, ErrState
		}
		if _, err = tx.Exec(`INSERT INTO assignments
SELECT ?,coalesce(max(generation),0)+1,'personal-worker',? FROM assignments WHERE task_id=?`,
			taskID, now(), taskID); err != nil {
			return Run{}, err
		}
	}
	if task.Status == "interrupted" && !task.RequirementsChanged {
		var runID string
		err = tx.QueryRow("SELECT id FROM runs WHERE task_id=? ORDER BY rowid DESC LIMIT 1", taskID).Scan(&runID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Run{}, err
		}
		var checkpoint string
		if runID != "" {
			checkpoint, err = readResumeStatus(tx, runID)
			if err != nil {
				return Run{}, err
			}
		}
		if retry && checkpoint != "retry" {
			return Run{}, ErrState
		}
		if !retry {
			switch checkpoint {
			case "ready":
				return resumeCheckpointRun(tx, runID, requestKey, fingerprint)
			case "retry", "unavailable":
				return Run{}, ErrReadCheckpoint
			}
		}
	}

	// Continue in a fresh Run with existing evidence and no repeat tool call.
	r := Run{ID: id(), TaskID: taskID, ConversationID: task.ConversationID, Status: "queued"}
	if _, err = tx.Exec("INSERT INTO runs(id,conversation_id,task_id,status,created_at) VALUES(?,?,?,?,?)", r.ID, r.ConversationID, taskID, r.Status, now()); err != nil {
		return Run{}, err
	}
	if task.RequirementsChanged {
		message, err := tx.Exec(`INSERT INTO messages(conversation_id,run_id,role,text,replace_materials)
VALUES(?,?,'user',?,coalesce((SELECT replace_materials FROM messages WHERE conversation_id=? AND role='user'
AND NOT EXISTS(SELECT 1 FROM runs WHERE id=messages.run_id AND memory_command=1) ORDER BY seq DESC LIMIT 1),0))`,
			task.ConversationID, r.ID, task.Goal, task.ConversationID)
		if err != nil {
			return Run{}, err
		}
		seq, err := message.LastInsertId()
		if err != nil {
			return Run{}, err
		}
		if err = recordInputEvent(tx, seq, requestKey); err != nil {
			return Run{}, err
		}
		if _, err = tx.Exec(`INSERT INTO message_materials(message_seq,material_id,ordinal)
SELECT ?,material_id,ordinal FROM message_materials WHERE message_seq=(
SELECT max(seq) FROM messages WHERE conversation_id=? AND role='user' AND seq<?
AND NOT EXISTS(SELECT 1 FROM runs WHERE id=messages.run_id AND memory_command=1))`, seq, task.ConversationID, seq); err != nil {
			return Run{}, err
		}
	} else if err = reuseTaskRead(tx, taskID, r.ID, task.Status != "cancelled"); err != nil {
		return Run{}, err
	}
	if _, err = tx.Exec("INSERT INTO submissions VALUES(?,?,?)", requestKey, fingerprint, r.ID); err != nil {
		return Run{}, err
	}
	reason := "等待继续处理"
	if retry {
		reason = "等待重新执行"
	}
	if task.RequirementsChanged {
		reason = "等待按新要求执行"
	}
	if _, err = tx.Exec("UPDATE tasks SET status='active',status_reason='queued',reason=?,requirements_changed=0,revision=revision+1,updated_at=? WHERE id=?", reason, now(), taskID); err != nil {
		return Run{}, err
	}
	if err = addEvent(tx, r.ID, "run.state_changed", map[string]string{"status": "queued"}); err != nil {
		return Run{}, err
	}
	return r, nil
}

func resumeCheckpointRun(tx *sql.Tx, runID, requestKey, fingerprint string) (Run, error) {
	if _, err := tx.Exec(`UPDATE runs SET status='queued',response='',error='',error_code='',control='',execution_generation=execution_generation+1
WHERE id=?`, runID); err != nil {
		return Run{}, err
	}
	if _, err := tx.Exec("INSERT INTO submissions VALUES(?,?,?)", requestKey, fingerprint, runID); err != nil {
		return Run{}, err
	}
	run, err := scanRun(tx.QueryRow("SELECT "+runColumns+" FROM runs WHERE id=?", runID))
	if err != nil {
		return Run{}, err
	}
	if run.TaskID != "" {
		if _, err = tx.Exec("UPDATE tasks SET status='active',status_reason='queued',reason='等待继续本轮工作',revision=revision+1,updated_at=? WHERE id=?", now(), run.TaskID); err != nil {
			return Run{}, err
		}
	}
	if err = addEvent(tx, runID, "message.reset", map[string]int{"execution_generation": run.Generation}); err != nil {
		return Run{}, err
	}
	if err = addEvent(tx, runID, "run.state_changed", map[string]string{"status": "queued"}); err != nil {
		return Run{}, err
	}
	return run, nil
}

func (s *Store) watchControl(ctx context.Context, runID string, started time.Time, cancel context.CancelFunc) {
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	lastCheckpoint := time.Now()
	for {
		var control string
		err := s.db.QueryRowContext(ctx, "SELECT control FROM runs WHERE id=? AND status='running' AND execution_generation=?", runID, executionGeneration(ctx)).Scan(&control)
		if err != nil || control != "" {
			cancel()
			return
		}
		if time.Since(lastCheckpoint) >= 2*time.Second {
			_, err := s.db.ExecContext(ctx, "UPDATE runs SET active_ms=max(active_ms,?) WHERE id=? AND status='running' AND execution_generation=?", time.Since(started).Milliseconds(), runID, executionGeneration(ctx))
			if err == nil {
				lastCheckpoint = time.Now()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
