package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

type Provider interface {
	Stream(context.Context, []Message, bool, int, func(string) error) (*TokenUsage, error)
}

type TaskAnswer struct {
	Question string
	Plan     []WorkflowStep
	Usage    *TokenUsage
}

type taskAnswerProvider interface {
	StreamTaskAnswer(context.Context, []Message, int, func(string) error) (TaskAnswer, error)
}

var ErrTaskAnswer = errors.New("模型未返回有效的事项结果")

func (s *Store) Recover() error {
	if err := s.recoverFinalizations(); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE run_read_checkpoints SET request_limit=0
WHERE request_limit>0 AND run_id IN (SELECT id FROM runs WHERE status='running')`); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE run_reads SET
status=CASE WHEN run_reads.status='dispatched' THEN 'unknown' WHEN r.control<>'stop' AND c.run_id IS NOT NULL THEN 'queued' ELSE 'not_run' END,
detail=CASE WHEN run_reads.status='dispatched' THEN '上次运行已中断，未收到完整的读取结果' WHEN r.control<>'stop' AND c.run_id IS NOT NULL THEN '' ELSE '上次运行已中断，工具未调用' END
FROM runs r LEFT JOIN run_read_checkpoints c ON c.run_id=r.id
WHERE run_reads.status IN ('queued','checking','dispatched') AND run_reads.run_id=r.id AND r.status='running'`); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE tasks SET status='interrupted',status_reason='reconciliation_required',reason='读取结果待确认',revision=revision+1,updated_at=?
WHERE id IN (SELECT r.task_id FROM runs r JOIN run_reads d ON d.run_id=r.id WHERE r.status='running' AND d.status='unknown')`, now()); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE runs SET status='interrupted',error='读取结果待确认',error_code='tool_read_unconfirmed',time_partial=usage_tracked
WHERE status='running' AND id IN (SELECT run_id FROM run_reads WHERE status='unknown')`); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE tasks SET status=CASE WHEN id IN (SELECT task_id FROM runs WHERE status='running' AND control='stop') THEN 'cancelled' ELSE 'interrupted' END,
status_reason=CASE (SELECT control FROM runs WHERE status='running' AND task_id=tasks.id) WHEN 'stop' THEN 'stopped' WHEN 'pause' THEN 'paused' ELSE 'process_interrupted' END,
reason=CASE (SELECT control FROM runs WHERE status='running' AND task_id=tasks.id) WHEN 'stop' THEN '已停止' WHEN 'pause' THEN '已暂停' ELSE '上次运行已中断' END,
revision=revision+1,updated_at=? WHERE id IN (SELECT task_id FROM runs WHERE status='running')`, now()); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE runs SET status=CASE WHEN control='stop' THEN 'cancelled' ELSE 'interrupted' END,
error=CASE control WHEN 'stop' THEN '已停止' WHEN 'pause' THEN '已暂停' ELSE '上次运行已中断' END,
error_code=CASE WHEN control='' THEN 'process_interrupted' ELSE control END,time_partial=usage_tracked WHERE status='running'`); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE connection_calls SET status='unknown' WHERE status='dispatched'"); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) claim(ctx context.Context) (Run, error) {
	// Failed outcomes retain their execution ownership until reconciled. Stop
	// new claims while any are pending, bounding the backlog by active workers.
	s.finalizationMu.Lock()
	pending := len(s.finalizations) > 0
	s.finalizationMu.Unlock()
	if pending {
		return Run{}, sql.ErrNoRows
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	run, err := scanRun(tx.QueryRow("SELECT " + runColumns + ` FROM runs WHERE status='queued'
AND NOT EXISTS(SELECT 1 FROM workflow_blocked_tasks w WHERE w.task_id=runs.task_id)
AND NOT EXISTS(SELECT 1 FROM run_queue_order q WHERE q.after_run_id=runs.id)
ORDER BY CASE (SELECT priority FROM tasks WHERE id=runs.task_id) WHEN 'high' THEN 0 WHEN 'low' THEN 2 ELSE 1 END,
rowid LIMIT 1`))
	if err != nil {
		return Run{}, err
	}
	if _, err = tx.Exec("UPDATE runs SET status='running',usage_tracked=1 WHERE id=?", run.ID); err != nil {
		return Run{}, err
	}
	if run.TaskID != "" {
		if _, err = tx.Exec("UPDATE tasks SET status_reason='running',reason='助手正在处理',revision=revision+1,updated_at=? WHERE id=?", now(), run.TaskID); err != nil {
			return Run{}, err
		}
	}
	if err = bindRun(tx, run); err != nil {
		return Run{}, err
	}
	if run.TaskID != "" {
		if err = bindWorkflowInputs(tx, run); err != nil {
			return Run{}, err
		}
	}
	if err = addEvent(tx, run.ID, "run.state_changed", map[string]string{"status": "running"}); err != nil {
		return Run{}, err
	}
	return run, tx.Commit()
}

func (s *Store) delta(ctx context.Context, id, text string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec("UPDATE runs SET response=response||? WHERE id=? AND status='running' AND control='' AND execution_generation=?", text, id, executionGeneration(ctx))
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrState
	}
	if err = addEvent(tx, id, "message.delta", map[string]any{"delta": text, "execution_generation": executionGeneration(ctx)}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) finish(run Run, status, failure string, accounting runAccounting) error {
	return s.finishContext(context.Background(), run, status, failure, accounting)
}

func (s *Store) finishContext(ctx context.Context, run Run, status, failure string, accounting runAccounting) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var control string
	if err = tx.QueryRowContext(ctx, `SELECT r.control FROM runs r
LEFT JOIN run_bindings b ON b.run_id=r.id AND b.generation=r.execution_generation
WHERE r.id=? AND r.status='running' AND r.execution_generation=?
AND (b.task_id IS NULL OR b.assignment_generation=(SELECT max(generation) FROM assignments WHERE task_id=b.task_id))`,
		run.ID, run.Generation).Scan(&control); err != nil {
		return err
	}
	if status == "completed" && control == "" && run.TaskID != "" {
		if err := checkWorkflowEvidence(tx, run.ID, run.Generation); errors.Is(err, ErrWorkflowEvidence) {
			status, failure, accounting.errorCode = "interrupted", err.Error(), "workflow_evidence_changed"
		} else if err != nil {
			return err
		}
	}
	preservePending := control == "pause" || (control != "stop" && status == "interrupted" &&
		(accounting.errorCode == "process_interrupted" || accounting.errorCode == "model_timeout" || accounting.errorCode == "model_error" || accounting.errorCode == "tool_read_failed"))
	if _, err = tx.ExecContext(ctx, `UPDATE run_reads SET status=CASE WHEN status='dispatched' THEN 'unknown' ELSE 'not_run' END,
detail=CASE WHEN status='dispatched' THEN '未保存完整的读取结果' ELSE '本轮未调用工具' END
WHERE run_id=? AND status IN ('queued','checking','dispatched')
AND (status='dispatched' OR NOT ? OR NOT EXISTS(SELECT 1 FROM run_read_checkpoints WHERE run_id=run_reads.run_id))`, run.ID, preservePending); err != nil {
		return err
	}
	if preservePending {
		if _, err = tx.ExecContext(ctx, `UPDATE run_reads SET status='queued',detail='' WHERE run_id=? AND status='checking'
AND EXISTS(SELECT 1 FROM run_read_checkpoints WHERE run_id=run_reads.run_id)`, run.ID); err != nil {
			return err
		}
	}
	if err = settleReadRequest(tx, run.ID, accounting.tokens); err != nil {
		return err
	}
	taskStatus, code, reason := "interrupted", "run_failed", failure
	switch accounting.errorCode {
	case "run_time_budget", "output_budget", "model_input_budget":
		code = "budget_exhausted"
	case "process_interrupted":
		code = "process_interrupted"
	}
	if control != "" {
		status, taskStatus, code, reason = controlOutcome(control)
		failure = reason
		accounting.errorCode = control
	}
	if run.TaskID != "" {
		unknown, err := unknownReadRun(tx, run.TaskID)
		if err != nil {
			return err
		}
		if unknown != "" {
			status, taskStatus, code, reason = "interrupted", "interrupted", "reconciliation_required", "读取结果待确认"
			failure, accounting.errorCode = reason, "tool_read_unconfirmed"
		}
	}
	if err = recordModelUsage(tx, run.ID, accounting.tokens); err != nil {
		return err
	}
	var plan workerPlan
	if status == "completed" && accounting.plan != "" {
		plan, err = prepareWorkerPlan(tx, run, accounting.plan, accounting.activeMS)
		switch {
		case errors.Is(err, ErrWorkflow), errors.Is(err, ErrWorkerPlanBudget):
			status, failure, reason, accounting.errorCode = "interrupted", err.Error(), err.Error(), "worker_plan_rejected"
		case err != nil:
			return err
		default:
			taskStatus, code, reason = "active", "waiting_dependencies", "等待子任务成果验收"
		}
	}
	planned := status == "completed" && len(plan.request.Steps) != 0
	if status == "completed" && run.TaskID != "" && accounting.question != "" {
		status, taskStatus, code, reason = "input_required", "waiting_user", "input_required", "等你补充信息"
	}
	if _, err = tx.ExecContext(ctx, `UPDATE runs SET status=?,error=?,error_code=?,active_ms=max(active_ms,?),
response=CASE WHEN ?='input_required' THEN ? ELSE response END WHERE id=?`,
		status, failure, accounting.errorCode, accounting.activeMS, status, accounting.question, run.ID); err != nil {
		return err
	}
	var response string
	if (status == "completed" || status == "input_required") && !planned {
		if err = tx.QueryRowContext(ctx, "SELECT response FROM runs WHERE id=?", run.ID).Scan(&response); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO messages(conversation_id,run_id,role,text) VALUES(?,?,'assistant',?)", run.ConversationID, run.ID, response); err != nil {
			return err
		}
	}
	if run.TaskID != "" {
		if status == "completed" && !planned {
			taskStatus, code, reason = "waiting_user", "acceptance_required", "成果已生成，等待你验收"
			var requiredFile string
			if err = tx.QueryRowContext(ctx, "SELECT required_filename FROM tasks WHERE id=?", run.TaskID).Scan(&requiredFile); err != nil {
				return err
			}
			if requiredFile != "" {
				code, reason = "delivery_required", "成果已生成，等待保存到指定文件"
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO artifacts VALUES(?,?,?,?,?,?,?)", id(), run.ID, run.TaskID, "助手成果.md", response, digest(response), now()); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, "UPDATE tasks SET status=?,status_reason=?,reason=?,revision=revision+1,updated_at=? WHERE id=?", taskStatus, code, reason, now(), run.TaskID); err != nil {
			return err
		}
	}
	if err = addEvent(tx, run.ID, "run.state_changed", map[string]string{"status": status, "error": failure, "error_code": accounting.errorCode}); err != nil {
		return err
	}
	if err = settleInputs(tx, run, status); err != nil {
		return err
	}
	if planned {
		if err = commitWorkerPlan(ctx, tx, run, plan); err != nil {
			return err
		}
	} else if run.TaskID != "" && (status == "completed" || status == "input_required") {
		if err = dispatchTaskFollowup(tx, run); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM run_finalizations WHERE run_id=? AND execution_generation=?", run.ID, run.Generation); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Start(ctx context.Context, provider Provider, concurrency int, limits RunLimits) *sync.WaitGroup {
	s.configureRuntime(concurrency, limits)
	var workers sync.WaitGroup
	workers.Go(func() { s.runGoalScheduler(ctx) })
	workers.Go(func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.reconcileFinalizations(ctx)
			}
		}
	})
	for range concurrency {
		workers.Go(func() {
			ticker := time.NewTicker(200 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					run, err := s.claim(ctx)
					if errors.Is(err, sql.ErrNoRows) || errors.Is(err, context.Canceled) {
						continue
					}
					if err != nil {
						slog.Error("claim run", "error", err)
						continue
					}
					s.execute(ctx, provider, run, limits)
				}
			}
		})
	}
	return &workers
}

func (s *Store) execute(ctx context.Context, provider Provider, run Run, limits RunLimits) {
	var prior time.Duration
	if run.Usage != nil {
		prior = time.Duration(run.Usage.ActiveMS) * time.Millisecond
	}
	limits, checkpointErr := s.applyPlanBudget(ctx, run, limits)
	var automatic bool
	if checkpointErr == nil && run.Generation > 0 {
		var limitMS int64
		checkpointErr = s.db.QueryRowContext(ctx, "SELECT active_budget_ms,automatic_tools FROM run_read_checkpoints WHERE run_id=?", run.ID).Scan(&limitMS, &automatic)
		if checkpointErr == nil {
			limits.ActiveTime = min(limits.ActiveTime, time.Duration(limitMS)*time.Millisecond)
		} else if errors.Is(checkpointErr, sql.ErrNoRows) {
			checkpointErr = ErrReadCheckpoint
		}
	}
	started := time.Now().Add(-prior)
	runCtx, endRun := context.WithTimeoutCause(ctx, limits.ActiveTime-prior, errRunBudget)
	defer endRun()
	controlled, cancel := context.WithCancel(context.WithValue(runCtx, executionGenerationKey{}, run.Generation))
	defer cancel()
	controlled = s.withManifestRecorder(controlled, run)
	go s.watchControl(controlled, run.ID, started, cancel)

	var input []Message
	var isolated IsolatedReplyPin
	var isolatedStream Provider
	err := checkpointErr
	var release func()
	if err == nil {
		release, err = s.acquireModelSlot(controlled)
	}
	limits.OutputTokens -= run.admissionOutput
	if err == nil && limits.OutputTokens < 16 {
		err = ErrOutputBudget
	}
	if err == nil {
		isolated, input, err = s.isolatedReplyInput(controlled, run)
		if err == nil && isolated.ProfileID == "" {
			input, err = s.modelMessagesForRun(controlled, run.ConversationID, run.ID)
		}
	}
	var request *Message
	for i := len(input) - 1; i >= 0; i-- {
		if input[i].Role == "user" {
			request = &input[i]
			break
		}
	}
	if err == nil && request == nil {
		err = errors.New("run has no user message")
	}
	var query string
	if request != nil {
		query = request.Text
	}
	var restrictAutoTools bool
	if err == nil && run.TaskID != "" {
		var goal, criteria, filename string
		err = s.db.QueryRowContext(controlled, `SELECT goal,acceptance_criteria,required_filename,
EXISTS(SELECT 1 FROM goal_tasks WHERE task_id=tasks.id)
OR EXISTS(SELECT 1 FROM workflow_children WHERE task_id=tasks.id OR parent_id=tasks.id)
FROM tasks WHERE id=?`, run.TaskID).Scan(&goal, &criteria, &filename, &restrictAutoTools)
		if err == nil {
			if request.Text != goal {
				request.Text += "\n\n当前事项的原始目标：\n" + goal
				query += "\n" + goal
			}
			if criteria != "" {
				request.Text += "\n\n当前事项的验收条件：\n" + criteria
			}
			if filename != "" {
				request.Text += "\n\n请直接生成交付文件的正文。应用会在用户确认后保存到 outputs/" + filename + "。"
			}
		}
	}
	if err == nil && run.TaskID != "" {
		err = s.attachWorkflowInputs(controlled, run, request)
	}
	if err == nil && run.TaskID != "" {
		request.Experience, err = s.routeTaskExperience(controlled, run)
	}
	if err == nil && run.TaskID != "" {
		request.Skill, err = s.skillForRun(controlled, run)
	}
	var readModel TaskReadModel
	var checkpointReadModel MultiTaskReadModel
	var checkpointStream Provider
	var providerBinding string
	var reads []TaskRead
	if err == nil && run.TaskID != "" {
		reads, err = listTaskReads(controlled, s.db, run.ID)
		if err == nil && !automatic {
			if len(reads) == 0 || reads[0].Status == "reused" {
				var evidence []TaskRead
				evidence, err = includeEarlierReadFailures(controlled, s.db, run.ID, reads)
				if err == nil {
					if len(evidence) == 1 {
						request.Read = &evidence[0]
					} else {
						request.Reads = evidence
					}
				}
			} else {
				if native, ok := provider.(multiTaskReadProvider); ok {
					checkpointReadModel, err = native.MultiTaskReadModel(controlled)
				}
				if err == nil && checkpointReadModel != nil {
					providerBinding, _ = checkpointReadModel.ReadCheckpointIdentity()
					if run.Generation == 0 && limits.OutputTokens < 16*(len(reads)+1) {
						err = ErrOutputBudget
						if len(reads) == 1 {
							err = errReadOutputBudget
						}
					}
				}
				if err == nil && checkpointReadModel == nil && run.Generation == 0 {
					if len(reads) == 1 {
						if native, ok := provider.(taskReadProvider); ok {
							readModel, err = native.TaskReadModel(controlled)
						}
						if err == nil && readModel == nil {
							request.Read, err = s.runTaskRead(controlled, run.ID)
						}
						if err == nil && readModel != nil && limits.OutputTokens < 32 {
							err = errReadOutputBudget
						}
					} else {
						request.Reads, err = s.readTaskSources(controlled, run.ID, reads)
					}
				}
			}
		}
	}
	if err == nil && isolated.ProfileID == "" && (automatic || len(reads) == 0 || reads[0].Status == "reused") {
		if native, ok := provider.(textCheckpointProvider); ok {
			checkpointStream, providerBinding, err = native.TextRunProvider(controlled, run.TaskID != "")
		}
	}
	var tools []taskTool
	automaticModel, supportsTools := checkpointStream.(automaticTaskToolModel)
	if err == nil && run.TaskID != "" && !restrictAutoTools && (automatic || (supportsTools && run.Generation == 0 && len(reads) == 0)) {
		if !supportsTools {
			err = ErrReadCheckpoint
		} else {
			tools, err = s.automaticTaskTools(controlled, run)
			automatic = automatic || len(tools) > 0
			if automatic {
				providerBinding += "\n" + automaticReadProtocol
			}
		}
	}
	checkpointed := checkpointReadModel != nil || checkpointStream != nil
	var remainingOutput int
	if err == nil {
		switch {
		case isolated.ProfileID != "":
			var native isolatedReplyProvider
			native, _ = provider.(isolatedReplyProvider)
			if native == nil {
				err = ErrIsolatedReplyUnavailable
			} else if _, err = s.prepareRunRequest(controlled, run.ID, time.Since(started), "", false); err == nil {
				isolatedStream, err = native.IsolatedReplyProvider(controlled, isolated)
				if err == nil && isolatedStream == nil {
					err = ErrIsolatedReplyUnavailable
				}
			}
		case checkpointed:
			remainingOutput, err = s.prepareReadCheckpoint(controlled, run, input, request, query, providerBinding, limits, automatic)
		case run.Generation > 0:
			err = ErrReadCheckpoint
		default:
			request.Memories, err = s.prepareModelRequest(controlled, run.ID, time.Since(started), query)
		}
	}
	var accounting runAccounting
	if err == nil && checkpointStream != nil && remainingOutput >= 512 {
		controlled, err = s.workerPlanContext(controlled, run, limits)
	}
	if err == nil && run.TaskID != "" {
		err = s.checkTaskReadModelAuthority(controlled, run.ID)
	}
	if err == nil {
		var answer TaskAnswer
		size := 0
		emit := func(modelCtx context.Context, text string) error {
			size += len(text)
			if size > maxOutputBytes {
				return ErrOutputBudget
			}
			return s.delta(modelCtx, run.ID, text)
		}
		switch {
		case isolatedStream != nil:
			answer, err = streamAnswer(controlled, isolatedStream, input, false, limits.OutputTokens, limits.ModelTimeout, emit)
		case automatic:
			answer, err = s.streamWithAutomaticTools(controlled, run.ID, started, automaticModel, input, tools, reads, remainingOutput, limits, emit)
		case checkpointReadModel != nil:
			answer, err = s.streamWithTaskReads(controlled, run.ID, started, checkpointReadModel, input, reads, remainingOutput, limits, emit)
		case readModel != nil:
			answer, err = s.streamWithTaskRead(controlled, run.ID, started, readModel, input, limits, emit)
		case checkpointStream != nil:
			limit := recoverableAnswerLimit(remainingOutput, run.Generation)
			if limit < 16 {
				err = ErrOutputBudget
			} else if err = s.reserveReadRequest(controlled, run.ID, limit, time.Since(started)); err == nil {
				answer, err = streamAnswer(controlled, checkpointStream, input, run.TaskID != "", limit, limits.ModelTimeout, emit)
				if err == nil && answer.Usage != nil && answer.Usage.OutputTokens != nil && *answer.Usage.OutputTokens > int64(limit) {
					err = ErrOutputBudget
				}
			}
		default:
			answer, err = streamAnswer(controlled, provider, input, run.TaskID != "", limits.OutputTokens, limits.ModelTimeout, emit)
		}
		accounting.tokens, accounting.question = answer.Usage, answer.Question
		if err == nil && len(answer.Plan) != 0 {
			if !WorkerCanPlan(controlled) || size != 0 || answer.Question != "" {
				err = ErrTaskAnswer
			} else {
				var payload []byte
				payload, err = json.Marshal(answer.Plan)
				accounting.plan = string(payload)
			}
		}
		if err == nil && size == 0 && accounting.question == "" && accounting.plan == "" {
			err = errors.New("empty response")
		}
	}
	if cause := context.Cause(controlled); err != nil && cause != nil {
		err = cause
	}
	status, reason := "completed", ""
	if err != nil {
		slog.Warn("run interrupted", "run_id", run.ID, "error", err)
		status = "interrupted"
		switch {
		case errors.Is(err, errRunBudget):
			reason, accounting.errorCode = "已达到本轮执行时间上限", "run_time_budget"
		case errors.Is(err, errModelTimeout):
			reason, accounting.errorCode = "模型请求超时，请继续工作或补充要求后重试", "model_timeout"
		case errors.Is(err, ErrOutputBudget):
			reason, accounting.errorCode = "已达到输出上限，请缩小本轮交付范围后重试", "output_budget"
		case errors.Is(err, ErrImageUnsupported):
			reason, accounting.errorCode = ErrImageUnsupported.Error(), "model_image_unsupported"
		case errors.Is(err, ErrModelRequestBudget):
			reason, accounting.errorCode = "本轮模型请求超过输入上限，请缩小委托范围或调整输入上限", "model_input_budget"
		case errors.Is(err, errReadOutputBudget):
			reason, accounting.errorCode = "工具读取需要至少 32 个输出 Token，请调整启动配置", "output_budget"
		case errors.Is(err, ErrReadModelCall):
			reason, accounting.errorCode = "模型未发出有效的读取调用，请检查模型工具调用能力", "invalid_model_tool_call"
		case errors.Is(err, ErrTaskAnswer):
			reason, accounting.errorCode = ErrTaskAnswer.Error(), "invalid_task_answer"
		case errors.Is(err, ErrReadNeedsSelection):
			reason, accounting.errorCode = ErrReadNeedsSelection.Error(), "tool_read_reselection_required"
		case errors.Is(err, errReadFailed):
			reason, accounting.errorCode = "读取未完成，请检查工具状态并重新提交", "tool_read_failed"
		case ctx.Err() != nil:
			reason, accounting.errorCode = "运行已中断", "process_interrupted"
		case errors.Is(err, ErrIsolatedReplyUnavailable):
			status, reason, accounting.errorCode = "failed", ErrIsolatedReplyUnavailable.Error(), "isolated_reply_unavailable"
		case errors.Is(err, ErrReadCheckpoint):
			reason, accounting.errorCode = ErrReadCheckpoint.Error(), "read_checkpoint_unavailable"
		case checkpointed:
			reason, accounting.errorCode = "未取得完整回复", "model_error"
		default:
			status, reason, accounting.errorCode = "failed", "未取得完整回复，请检查模型连接后重试", "model_error"
		}
	}
	accounting.activeMS = time.Since(started).Milliseconds()
	if release != nil {
		release()
	}
	// Persistence retries do not consume execution time or keep control polling
	// alive. Every retry reads the persisted control and generation in finish.
	cancel()
	s.finalizeRun(run, status, reason, accounting)
}

type runFinalizationKey struct {
	runID      string
	generation int
}

type runFinalization struct {
	run        Run
	status     string
	failure    string
	accounting runAccounting
	inFlight   bool
}

func (s *Store) finalizeRun(run Run, status, failure string, accounting runAccounting) {
	key := runFinalizationKey{run.ID, run.Generation}
	outcome := &runFinalization{run: run, status: status, failure: failure, accounting: accounting, inFlight: true}
	s.finalizationMu.Lock()
	if s.finalizations == nil {
		s.finalizations = make(map[runFinalizationKey]*runFinalization)
	}
	s.finalizations[key] = outcome
	s.finalizationMu.Unlock()
	// A worker makes only three bounded persistence attempts. The independent
	// reconciler owns any remaining outcome; it never replays model/tool work.
	var err error
	for range 3 {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err = s.persistFinalization(ctx, run, status, failure, accounting)
		cancel()
		if err == nil || errors.Is(err, sql.ErrNoRows) {
			break
		}
	}
	s.releaseFinalization(key, outcome, err)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		slog.Error("run outcome awaits persistence recovery", "run_id", run.ID, "generation", run.Generation, "error", err)
	}
}

func (s *Store) releaseFinalization(key runFinalizationKey, outcome *runFinalization, err error) {
	s.finalizationMu.Lock()
	defer s.finalizationMu.Unlock()
	if s.finalizations[key] != outcome {
		return
	}
	if err == nil || errors.Is(err, sql.ErrNoRows) {
		// A non-running/changed-generation Run was already finalized or recovered.
		// Its old outcome must never advance the new execution or add usage twice.
		delete(s.finalizations, key)
	} else {
		outcome.inFlight = false
	}
}

func (s *Store) reconcileFinalizations(ctx context.Context) {
	s.finalizationMu.Lock()
	batch := make(map[runFinalizationKey]*runFinalization)
	for key, outcome := range s.finalizations {
		if !outcome.inFlight && len(batch) < 8 {
			outcome.inFlight = true
			batch[key] = outcome
		}
	}
	s.finalizationMu.Unlock()
	// The entire batch has a deadline, including shutdown. If the process exits
	// first, startup Recover settles durable receipts before interrupting Runs
	// without a confirmed outcome.
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	for key, outcome := range batch {
		err := s.persistFinalization(ctx, outcome.run, outcome.status, outcome.failure, outcome.accounting)
		s.releaseFinalization(key, outcome, err)
		if err == nil {
			slog.Info("recovered run outcome persistence", "run_id", key.runID, "generation", key.generation)
		}
	}
}

func streamAnswer(ctx context.Context, provider Provider, input []Message, task bool, limit int, timeout time.Duration, emit func(context.Context, string) error) (TaskAnswer, error) {
	modelCtx, cancel := context.WithTimeoutCause(ctx, timeout, errModelTimeout)
	defer cancel()
	emitText := func(text string) error { return emit(modelCtx, text) }
	var answer TaskAnswer
	var err error
	if model, ok := provider.(taskAnswerProvider); task && ok {
		answer, err = model.StreamTaskAnswer(modelCtx, input, limit, emitText)
	} else {
		answer.Usage, err = provider.Stream(modelCtx, input, task, limit, emitText)
	}
	if cause := context.Cause(modelCtx); cause != nil {
		return answer, cause
	}
	return answer, err
}

func (s *Store) Events(runID string, after int64) ([]Event, Run, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, Run{}, err
	}
	defer tx.Rollback()
	run, err := scanRun(tx.QueryRow("SELECT "+runColumns+" FROM runs WHERE id=?", runID))
	if err != nil {
		return nil, Run{}, err
	}
	var privateReply bool
	if err := tx.QueryRow("SELECT "+revokedMCPReply("?"), runID).Scan(&privateReply); err != nil {
		return nil, Run{}, err
	}
	rows, err := tx.Query("SELECT seq,kind,data FROM run_events WHERE run_id=? AND seq>? ORDER BY seq LIMIT 128", runID, after)
	if err != nil {
		return nil, Run{}, err
	}
	var events []Event
	for rows.Next() {
		var e Event
		if err = rows.Scan(&e.Seq, &e.Kind, &e.Data); err != nil {
			rows.Close()
			return nil, Run{}, err
		}
		if privateReply && e.Kind == "message.delta" {
			e.Kind, e.Data = "message.content_unavailable", `{"reason":"source_unavailable"}`
		}
		events = append(events, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, Run{}, fmt.Errorf("read events: %w", err)
	}
	return events, run, tx.Commit()
}
