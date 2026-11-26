package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const benchmarkCallTimeout = 10 * time.Second
const benchmarkClaimMS = int64(30000)

// A tick may defer this many busy queue entries but never starts more than
// one provider call. A finite budget also bounds returned work on busy queues.
const benchmarkScanBudget = 16

// This is a one-shot query recheck, not a remote execution limit.
const benchmarkWaitRecheckMS = int64((5 * time.Minute) / time.Millisecond)

type benchmarkDispatch struct {
	request                                   BenchmarkJobRequest
	provider                                  BenchmarkProvider
	spec                                      BenchmarkCase
	step, task, actor, operation, job, action string
	jobSource                                 string
	generation                                int
	deferredForHold                           bool
}

func (s *Store) runBenchmarkOnce(ctx context.Context, provider BenchmarkProvider) (bool, error) {
	if provider == nil {
		return false, nil
	}
	if err := s.applyBenchmarkTimeouts(ctx, now()); err != nil {
		return false, err
	}
	if err := s.applyBenchmarkHints(ctx, now()); err != nil {
		return false, err
	}
	if err := s.applyAccountBenchmarkHints(ctx, now()); err != nil {
		return false, err
	}
	// A fixed cutoff excludes entries just deferred in this same tick.
	// Each new transaction still checks live expiry/authorization against
	// its own clock and issues only the original kind of provider action.
	cutoff := now()
	var d benchmarkDispatch
	var worked bool
	var err error
	for i := 0; i < benchmarkScanBudget; i++ {
		var selected bool
		d, selected, err = s.claimBenchmarkDue(ctx, provider, cutoff, now())
		worked = worked || selected
		if err != nil || !selected {
			return worked, err
		}
		if !d.deferredForHold {
			break
		}
	}
	if d.action == "" {
		return worked, nil
	}
	call, cancel := context.WithTimeout(ctx, benchmarkCallTimeout)
	defer cancel()
	var confirmed string
	var result BenchmarkJobResult
	switch d.action {
	case "start":
		confirmed, err = d.provider.Start(call, d.request)
	case "lookup":
		confirmed, err = d.provider.Lookup(call, d.request)
	case "get":
		result, err = d.provider.Get(call, d.request, d.job)
	case "cancel":
		err = d.provider.(benchmarkCanceller).Cancel(call, d.request, d.job)
	}
	// Never retry Start. If the process dies here, the durable row only
	// permits lookup using the original idempotency key.
	return true, s.settleBenchmark(ctx, d, confirmed, result, err, now())
}

func (s *Store) claimBenchmark(ctx context.Context, provider BenchmarkProvider, stamp int64) (benchmarkDispatch, bool, error) {
	return s.claimBenchmarkDue(ctx, provider, stamp, stamp)
}

func (s *Store) claimBenchmarkDue(ctx context.Context, provider BenchmarkProvider, cutoff, stamp int64) (benchmarkDispatch, bool, error) {
	var d benchmarkDispatch
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return d, false, err
	}
	defer tx.Rollback()
	instance := s.benchmarkInstance
	var principal string
	if instance != nil {
		principal, err = lockBenchmarkAccount(ctx, tx)
		if err != nil {
			return d, false, err
		}
		if err = reapAccountBenchmarkHolds(ctx, tx, instance, principal); err != nil {
			return d, false, err
		}
	}
	var profile, phase, targetsRaw, authsRaw, caseSpec string
	var version int
	var stopping, cancelSent bool
	// Zero means immediately eligible for a queued case, not oldest work in
	// the database. Sort by its persisted enqueue/revision and prerequisite
	// settlement times so new submissions cannot overtake due reconciliation.
	dispatchPin := accountBenchmarkDispatchPin(provider)
	if dispatchPin != "" && instance == nil {
		return d, false, ErrAuthorizationDenied
	}
	err = tx.QueryRow(`SELECT b.step_id,b.task_id,r.actor,r.profile_key,r.profile_version,b.phase,b.operation_id,b.job_id,b.generation,
b.case_key,b.frozen_targets,b.authorization_refs,r.stop_requested,b.cancel_sent,b.case_spec FROM benchmark_cases b
JOIN benchmark_runs r ON r.task_id=b.task_id JOIN tasks t ON t.id=b.task_id
JOIN task_states ts ON ts.task_id=b.task_id JOIN steps s ON s.step_id=b.step_id
WHERE b.next_at<=? AND (?='' OR EXISTS(SELECT 1 FROM benchmark_account_dispatch_admissions a WHERE a.task_id=b.task_id AND a.provider_pin=?))
 AND (b.phase IN ('submitting','following') OR
  (b.phase='queued' AND (
    r.stop_requested=1 OR t.status='cancelled' OR (t.status='active' AND ts.state IN ('pending','running')
    AND NOT EXISTS(SELECT 1 FROM benchmark_dispatch_gates g WHERE g.step_id=b.step_id AND g.held=1)
    AND NOT EXISTS (
      SELECT 1 FROM step_dependencies e JOIN benchmark_cases p ON p.step_id=e.depends_on_step_id
      WHERE e.step_id=b.step_id AND p.phase NOT IN ('verified','incomplete','skipped')
    ))
  )))
ORDER BY CASE WHEN b.phase='queued' AND b.next_at=0 THEN max(s.updated_at, coalesce((
    SELECT max(dep.updated_at) FROM step_dependencies e JOIN steps dep ON dep.step_id=e.depends_on_step_id
    WHERE e.step_id=b.step_id
  ),0)) ELSE b.next_at END,
  r.created_at, s.position, b.step_id LIMIT 1`, cutoff, dispatchPin, dispatchPin).
		Scan(&d.step, &d.task, &d.actor, &profile, &version, &phase, &d.operation, &d.job, &d.generation, &d.request.Case, &targetsRaw, &authsRaw, &stopping, &cancelSent, &caseSpec)
	if errors.Is(err, sql.ErrNoRows) {
		return d, false, nil
	}
	if err != nil {
		return d, false, err
	}
	if instance != nil && principal != d.actor {
		return d, false, ErrAuthorizationDenied
	}
	if caseSpec != "" {
		if err := json.Unmarshal([]byte(caseSpec), &d.spec); err != nil {
			return d, false, err
		}
	} else {
		p, err := readBenchmarkProfile(tx, profile, version)
		if err != nil {
			return d, false, err
		}
		for _, c := range p.Cases {
			if c.Key == d.request.Case {
				d.spec = c
				break
			}
		}
	}
	if d.spec.Key == "" || d.spec.Key != d.request.Case {
		return d, false, ErrState
	}
	d.request = benchmarkCaseRequest(d.spec, "benchmark:"+d.step)
	selection, selectErr := selectBenchmarkProvider(ctx, tx, provider, d.request.Binding)
	if selectErr != nil && !errors.Is(selectErr, ErrBenchmarkBinding) {
		return d, false, selectErr
	}
	d.provider = selection.provider
	d.jobSource = selection.jobSource
	unboundProvider := instance != nil && selection.jobSource == ""
	if phase == "queued" {
		reason := ""
		var revoked bool
		if err = tx.QueryRow(`SELECT t.status='cancelled'
OR EXISTS(SELECT 1 FROM step_dependencies e JOIN benchmark_cases p ON p.step_id=e.depends_on_step_id
WHERE e.step_id=? AND p.phase IN ('incomplete','skipped'))
OR (SELECT max(revision) FROM task_revisions WHERE task_id=t.id)<>1
FROM tasks t WHERE id=?`, d.step, d.task).Scan(&revoked); err != nil {
			return d, false, err
		}
		if revoked || stopping {
			reason = "任务已取消、要求已修改或必需依赖未核实"
		}
		if dispatchPin != "" {
			var active bool
			if err = instance.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE principal_id=? AND status='active' AND must_change_password=0)`, principal).Scan(&active); err != nil {
				return d, false, err
			}
			if !active {
				reason = "所属账号已停用或需要重新设置密码，不再派发新作业"
			}
		}
		if selectErr != nil {
			reason = "冻结的基准连接或工具不可用"
		} else if unboundProvider {
			reason = "基准提供方尚未建立可验证的实例作业身份"
		} else if err := selection.validate(d.request); err != nil {
			reason = err.Error()
		}
		var targets []ticketTarget
		var auths map[string]string
		var physicalSerials []string
		seenSerials := make(map[string]bool)
		if err = json.Unmarshal([]byte(targetsRaw), &targets); err != nil {
			return d, false, err
		}
		if err = json.Unmarshal([]byte(authsRaw), &auths); err != nil {
			return d, false, err
		}
		for _, target := range targets {
			if reason != "" {
				break
			}
			a, e := benchmarkAsset(tx, target.Ref)
			if e != nil && !errors.Is(e, ErrState) && !errors.Is(e, ErrBenchmarkInput) {
				return d, false, e
			}
			if e != nil || a.Revision != target.Revision || a.OwnershipEpoch != target.Epoch {
				reason = "目标身份、占用条件或版本已改变"
				break
			}
			e = authorizeFrozenBenchmarkGrant(tx, d.actor, target.Ref, d.spec, stamp, auths[target.Ref])
			if e != nil && !errors.Is(e, ErrAuthorizationDenied) {
				return d, false, e
			}
			if e != nil {
				reason = "冻结的执行授权已失效"
				break
			}
			if instance != nil {
				var serial string
				err = tx.QueryRowContext(ctx, "SELECT physical_serial FROM benchmark_asset_physical_serials WHERE target_ref=?", target.Ref).Scan(&serial)
				if errors.Is(err, sql.ErrNoRows) {
					reason = "测试目标未登记可信的物理序列号"
					break
				}
				if err != nil {
					return d, false, err
				}
				if seenSerials[serial] {
					reason = "测试方案重复引用同一物理资产"
					break
				}
				seenSerials[serial] = true
				physicalSerials = append(physicalSerials, serial)
			}
			held, holdErr := targetHasLocalExclusiveHold(tx, target.Ref)
			if holdErr != nil {
				return d, false, holdErr
			}
			if held {
				d.deferredForHold = true
				_, err = tx.Exec("UPDATE benchmark_cases SET next_at=? WHERE step_id=?", stamp+1000, d.step)
				if err != nil {
					return d, false, err
				}
				return d, true, tx.Commit()
			}
		}
		if reason != "" {
			if err = finishBenchmarkCase(tx, d, "skipped", map[string]string{"reason": reason}, stamp); err != nil {
				return d, false, err
			}
			return d, true, tx.Commit()
		}
		if instance != nil {
			if _, err = tx.Exec("SAVEPOINT benchmark_queued_claim"); err != nil {
				return d, false, err
			}
		}
		current, err := readTaskState(tx, d.task)
		if err != nil {
			return d, false, err
		}
		if current.State == RuntimeTaskPending {
			if _, err = transitionTaskState(tx, d.task, EventClaim, TaskGuard{Actor: "system", ExpectedStorageRevision: current.StorageRevision}, stamp); err != nil {
				return d, false, err
			}
		}
		capabilityKey, capabilityVersion, connectionID := selection.capabilityKey, selection.capabilityVersion, selection.connectionID
		if capabilityKey == "" {
			capabilityKey = "benchmark." + d.spec.Binding.Tool
			capabilityVersion, connectionID = d.spec.Binding.Revision, d.spec.Binding.Connection
		}
		o, err := registerOperation(tx, Operation{TaskID: d.task, StepID: d.step, CapabilityKey: capabilityKey,
			CapabilityVersion: capabilityVersion, ConnectionID: connectionID, TargetRefs: targetsRaw,
			ParamsDigest: selection.requestDigest(d.request), AuthorizationRef: digest(authsRaw), IdempotencyKey: d.request.IdempotencyKey}, stamp)
		if err != nil {
			return d, false, err
		}
		if _, err = dispatchOperationAttempt(tx, o.OperationID, stamp); err != nil {
			return d, false, err
		}
		for _, target := range targets {
			if _, err = tx.Exec(`INSERT INTO benchmark_target_holds VALUES(?,?)`, target.Ref, d.step); err != nil {
				return d, false, err
			}
		}
		d.operation, d.action, phase = o.OperationID, "start", "submitting"
		if _, err = tx.Exec("UPDATE steps SET state='running',updated_at=? WHERE step_id=?", stamp, d.step); err != nil {
			return d, false, err
		}
		if _, err = openWait(tx, Wait{WaitID: "benchmark:" + d.step, TaskID: d.task, StepID: d.step, Kind: WaitKindExternal,
			MatchKey: d.operation, WaitVersion: 1, OpenedAt: stamp, DeadlineAt: stamp + benchmarkWaitRecheckMS, TimeoutPolicy: "reconcile"}); err != nil {
			return d, false, err
		}
		if _, err = createTimer(tx, Timer{TimerID: "benchmark:" + d.step, WaitID: "benchmark:" + d.step,
			WaitVersion: 1, DueAt: stamp + benchmarkWaitRecheckMS, CreatedAt: stamp}); err != nil {
			return d, false, err
		}
		if instance != nil {
			reserved, err := reserveInstanceBenchmarkHolds(ctx, instance, physicalSerials, principal, d.step, d.operation)
			if err != nil {
				return d, false, err
			}
			if !reserved {
				if _, err = tx.Exec("ROLLBACK TO benchmark_queued_claim; RELEASE benchmark_queued_claim"); err != nil {
					return d, false, err
				}
				if _, err = tx.Exec("UPDATE benchmark_cases SET next_at=? WHERE step_id=?", stamp+1000, d.step); err != nil {
					return d, false, err
				}
				d.action, d.deferredForHold = "", true
				return d, true, tx.Commit()
			}
			if _, err = tx.Exec("RELEASE benchmark_queued_claim"); err != nil {
				return d, false, err
			}
		}
	} else {
		if instance != nil {
			var held int
			if err = instance.QueryRowContext(ctx, "SELECT count(*) FROM benchmark_instance_holds WHERE principal_id=? AND step_id=? AND operation_id=?", principal, d.step, d.operation).Scan(&held); err != nil {
				return d, false, err
			}
			if held == 0 || held != len(d.spec.Targets) {
				// This operation has no provable instance-wide ownership.
				// Neither start another job nor claim its resources implicitly.
				if _, err = tx.Exec("UPDATE benchmark_cases SET next_at=? WHERE step_id=?", stamp+30000, d.step); err != nil {
					return d, false, err
				}
				return d, true, tx.Commit()
			}
		}
		op, err := readOperation(tx, d.operation)
		if err != nil {
			return d, false, err
		}
		// Never send a query/cancel to another tool or transport after Start.
		// A pre-upgrade operation without a provable pin also stays unknown.
		validSelection := selectErr == nil && !unboundProvider && op.ParamsDigest == selection.requestDigest(d.request)
		if validSelection && instance != nil && phase == "following" {
			validSelection, err = instanceBenchmarkJobMatches(ctx, instance, d.instanceJob(d.job, op))
			if err != nil {
				return d, false, err
			}
		}
		if !validSelection {
			_, err = tx.Exec("UPDATE benchmark_cases SET next_at=? WHERE step_id=?", stamp+30000, d.step)
			if err != nil {
				return d, false, err
			}
			return d, true, tx.Commit()
		}
		d.action = "lookup"
		if phase == "following" {
			d.action = "get"
			if stopping && !cancelSent && selection.canStop {
				if _, err = tx.Exec("UPDATE benchmark_cases SET cancel_sent=1 WHERE step_id=?", d.step); err != nil {
					return d, false, err
				}
				d.action = "cancel"
			}
		}
	}
	d.generation++
	if _, err = tx.Exec(`UPDATE benchmark_cases SET phase=?,operation_id=?,generation=?,next_at=?,call_until=? WHERE step_id=?`, phase, d.operation, d.generation, stamp+benchmarkClaimMS, stamp+benchmarkClaimMS, d.step); err != nil {
		return d, false, err
	}
	return d, true, tx.Commit()
}

func (s *Store) settleBenchmark(ctx context.Context, d benchmarkDispatch, confirmed string, result BenchmarkJobResult, cause error, stamp int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var generation int
	if err = tx.QueryRow("SELECT generation FROM benchmark_cases WHERE step_id=?", d.step).Scan(&generation); err != nil {
		return err
	}
	if generation != d.generation {
		return nil
	}
	if _, err = tx.Exec("UPDATE benchmark_cases SET call_until=0 WHERE step_id=?", d.step); err != nil {
		return err
	}

	if d.action == "cancel" {
		summary := "已请求停止，等待提供方确认作业终态"
		if cause != nil {
			summary = "停止请求未确认，继续查询原作业"
		}
		if err = recordAudit(tx, AuditEvent{TaskID: d.task, StepID: d.step, DetailRef: d.operation,
			Event: "benchmark_cancel_requested", Actor: "system", Summary: summary, OccurredAt: stamp}); err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE benchmark_cases SET next_at=? WHERE step_id=?", stamp, d.step); err != nil {
			return err
		}
		return tx.Commit()
	}

	op, err := readOperation(tx, d.operation)
	if err != nil {
		return err
	}
	validJob := func(x string) bool { return validBenchKey(x) && x != "." && x != ".." && !strings.ContainsAny(x, "?#") }
	unknown := cause != nil
	if d.action == "start" || d.action == "lookup" {
		unknown = unknown || !validJob(confirmed)
	} else {
		unknown = unknown || result.JobID != d.job ||
			(result.State != "running" && result.State != "completed" && result.State != "failed" && result.State != "cancelled")
	}
	if !unknown && (d.action == "start" || d.action == "lookup") {
		var owner string
		existing := tx.QueryRow(`SELECT operation_id FROM benchmark_job_identities WHERE connection_key=? AND binding_revision=? AND provider_job_id=?`,
			op.ConnectionID, d.spec.Binding.Revision, confirmed).Scan(&owner)
		if existing != nil && !errors.Is(existing, sql.ErrNoRows) {
			return existing
		}
		unknown = existing == nil && owner != d.operation
		if !unknown && s.benchmarkInstance != nil {
			if op.TaskID != d.task || op.StepID != d.step || op.IdempotencyKey != d.request.IdempotencyKey {
				return ErrConflict
			}
			bound, err := bindInstanceBenchmarkJob(ctx, s.benchmarkInstance, d.instanceJob(confirmed, op))
			if err != nil {
				return err
			}
			unknown = !bound
		}
		if !unknown && existing != nil {
			if _, err = tx.Exec("INSERT INTO benchmark_job_identities VALUES(?,?,?,?)", op.ConnectionID, d.spec.Binding.Revision, confirmed, d.operation); err != nil {
				return err
			}
		}
	}

	if unknown {
		if op.State == RuntimeOperationDispatched || op.State == RuntimeOperationRunning {
			if _, err = transitionOperation(tx, d.operation, RuntimeOperationReconcile, stamp); err != nil {
				return err
			}
			if err = recordAudit(tx, AuditEvent{TaskID: d.task, StepID: d.step, DetailRef: d.operation, Event: "benchmark_pending_reconciliation",
				Actor: "system", Summary: "无法查证原作业，保留占用，不重发", OccurredAt: stamp}); err != nil {
				return err
			}
		}
		if _, err = tx.Exec("UPDATE benchmark_cases SET next_at=?,report=? WHERE step_id=?", stamp+jobBackoff(generation), `{"reason":"原作业结果待核对，不会重发"}`, d.step); err != nil {
			return err
		}
		return tx.Commit()
	}
	if d.action != "get" {
		if _, err = normalizeJobStart(tx, d.operation, confirmed, d.request.IdempotencyKey, "benchmark-v1", stamp); err != nil {
			return err
		}
		if op.State == RuntimeOperationDispatched {
			if _, err = transitionOperation(tx, d.operation, RuntimeOperationRunning, stamp); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(`UPDATE benchmark_cases SET job_id=?,phase='following',next_at=?,report='' WHERE step_id=?`, confirmed, stamp, d.step); err != nil {
			return err
		}
		return tx.Commit()
	}
	if result.State == "running" {
		if _, err = tx.Exec("UPDATE benchmark_cases SET next_at=? WHERE step_id=?", stamp+jobBackoff(generation), d.step); err != nil {
			return err
		}
		return tx.Commit()
	}
	assessment, err := verifyBenchmarkJob(tx, d, result)
	if err != nil {
		return err
	}
	to, jobState := RuntimeOperationSucceeded, "succeeded"
	switch result.State {
	case "failed":
		to, jobState = RuntimeOperationFailed, "failed"
	case "cancelled":
		to, jobState = RuntimeOperationCancelled, "failed"
	}
	if _, err = transitionOperation(tx, d.operation, to, stamp); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE operation_jobs SET state=?,updated_at=? WHERE operation_id=? AND provider_job_id=?", jobState, stamp, d.operation, d.job); err != nil {
		return err
	}
	if _, err = resolveWait(tx, "benchmark:"+d.step, 1, "benchmark-provider", d.operation+":terminal", "job_terminal", "", "system", stamp); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE timers SET state='cancelled' WHERE wait_id=? AND wait_version=1 AND state='pending'", "benchmark:"+d.step); err != nil {
		return err
	}
	phase := "incomplete"
	if result.State == "completed" && assessment.Complete {
		phase = "verified"
	}
	if err = finishBenchmarkCase(tx, d, phase, assessment, stamp); err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM benchmark_target_holds WHERE step_id=?", d.step); err != nil {
		return err
	}
	if err = recordBenchmarkAttentionTerminal(tx, d, stamp); err != nil {
		return err
	}
	return tx.Commit()
}

func finishBenchmarkCase(tx *sql.Tx, d benchmarkDispatch, phase string, report any, stamp int64) error {
	raw, err := json.Marshal(report)
	if err != nil {
		return err
	}
	stepState := RuntimeStepFailed
	if phase == "verified" {
		stepState = RuntimeStepCompleted
	} else if phase == "skipped" {
		stepState = RuntimeStepCancelled
	}
	if _, err = tx.Exec("UPDATE benchmark_cases SET phase=?,report=? WHERE step_id=?", phase, string(raw), d.step); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE steps SET state=?,updated_at=? WHERE step_id=?", stepState, stamp, d.step); err != nil {
		return err
	}
	if err = recordAudit(tx, AuditEvent{TaskID: d.task, StepID: d.step, Event: "benchmark_case_" + phase, Actor: "system", OccurredAt: stamp}); err != nil {
		return err
	}
	return finalizeBenchmarkIfSettled(tx, d.task, stamp)
}

func finalizeBenchmarkIfSettled(tx *sql.Tx, task string, stamp int64) error {
	var err error
	var unfinished, bad, stopping bool
	var revision int
	var legacy string
	if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM benchmark_cases WHERE task_id=t.id AND phase IN ('queued','submitting','following')),
EXISTS(SELECT 1 FROM benchmark_cases WHERE task_id=t.id AND phase IN ('incomplete','skipped')),t.status,r.stop_requested,(SELECT max(revision) FROM task_revisions rev WHERE rev.task_id=t.id) FROM tasks t JOIN benchmark_runs r ON r.task_id=t.id WHERE t.id=?`, task).Scan(&unfinished, &bad, &legacy, &stopping, &revision); err != nil {
		return err
	}
	if unfinished {
		return nil
	}
	cur, err := readTaskState(tx, task)
	if err != nil {
		return err
	}
	if cur.State.IsTerminal() || (cur.State == RuntimeTaskPaused && !stopping) {
		return nil
	}
	transition := func(event TaskEvent, result RuntimeTaskState) error {
		_, e := transitionTaskState(tx, task, event, TaskGuard{ExpectedStorageRevision: cur.StorageRevision, Actor: "system", FinalState: result, AcceptanceEvidence: true}, stamp)
		cur.StorageRevision++
		return e
	}
	final, reason := RuntimeTaskSucceeded, "已核实全部本次测量；性能达标单独记载"
	if legacy == "cancelled" || stopping {
		final, reason = RuntimeTaskCancelled, "已停止；在途作业已完成对账"
		if cur.State != RuntimeTaskFinalizing {
			if err = transition(EventCancel, ""); err != nil {
				return err
			}
		}
	} else if bad || revision != 1 {
		final, reason = RuntimeTaskFailed, "存在缺失、未核实或未执行的必测项"
		if err = transition(EventFail, ""); err != nil {
			return err
		}
	} else {
		if err = transition(EventStepsComplete, ""); err != nil {
			return err
		}
		if err = transition(EventAcceptancePass, ""); err != nil {
			return err
		}
	}
	if err = transition(EventFinalized, final); err != nil {
		return err
	}
	return projectTaskFromRuntimeState(tx, task, final, "benchmark_result", reason, stamp)
}

type benchReportJSON struct {
	benchmarkAssessment
	VerifiedMetrics map[string]map[string]benchmarkMetric `json:"verified_metrics"`
	RawArtifactIDs  map[string]string                     `json:"raw_artifact_ids,omitempty"`
	Comparison      *benchmarkComparison                  `json:"comparison,omitempty"`
}

func verifyBenchmarkJob(tx *sql.Tx, d benchmarkDispatch, r BenchmarkJobResult) (benchReportJSON, error) {
	job := benchmarkJob{ConnectionID: d.spec.Binding.Connection, OperationID: d.operation, ProviderJobID: d.job, Generation: 1}
	expected := make([]benchmarkRequirement, 0, len(d.spec.Targets))
	targets := map[string]bool{}
	for _, target := range d.spec.Targets {
		targets[target] = true
		e := benchmarkRequirement{Target: target, Check: d.spec.Key, Validator: d.spec.Binding.Validator, Job: job,
			ActualConfig: d.spec.ActualConfig, RequiredMetrics: d.spec.RequiredMetrics, NeedsRawEvidence: d.spec.NeedsRawEvidence}
		if t := d.spec.Threshold; t != nil {
			e.Threshold = &benchmarkThreshold{Metric: t.Metric, Unit: d.spec.RequiredMetrics[t.Metric], Minimum: t.Minimum}
		}
		expected = append(expected, e)
	}
	observed := make([]benchmarkObservation, 0, len(r.Measurements))
	total := 0
	if len(r.Measurements) > 2048 {
		r.Measurements = nil
	}
	for _, m := range r.Measurements {
		if !targets[m.Target] {
			continue
		}
		x := benchmarkObservation{Job: job, Target: m.Target, Check: d.spec.Key, Validator: d.spec.Binding.Validator, Outcome: m.Outcome,
			ActualConfig: m.ActualConfig, Metrics: map[string]benchmarkMetric{}}
		for key, mv := range m.Metrics {
			x.Metrics[key] = benchmarkMetric(mv)
		}
		if raw := m.Raw; raw != nil {
			x.Raw = &benchmarkRawEvidence{Job: job, Target: m.Target, Check: d.spec.Key}
			total += len(raw.Bytes)
			if total <= 8<<20 && len(raw.Bytes) > 0 && len(raw.Ref) <= 256 && raw.Ref != "" && digest(string(raw.Bytes)) == raw.SHA256 {
				artifactID := digest(toJSON([]string{d.operation, d.job, m.Target, raw.Ref, raw.SHA256}))
				if _, err := tx.Exec(`INSERT INTO benchmark_evidence(artifact_id,step_id,target_ref,provider_ref,sha256,body) VALUES(?,?,?,?,?,?)
ON CONFLICT(artifact_id) DO NOTHING`, artifactID, d.step, m.Target, raw.Ref, raw.SHA256, raw.Bytes); err != nil {
					return benchReportJSON{}, err
				}
				x.Raw = &benchmarkRawEvidence{Job: job, Target: m.Target, Check: d.spec.Key, Ref: artifactID, SHA256: raw.SHA256}
			}
		}
		if r.State != "completed" && x.Outcome == "measured" {
			x.Outcome = "failed"
		}
		observed = append(observed, x)
	}
	assessment, err := assessBenchmarkEvidence(expected, observed)
	if err != nil {
		return benchReportJSON{}, err
	}
	out := benchReportJSON{benchmarkAssessment: assessment, VerifiedMetrics: map[string]map[string]benchmarkMetric{}, RawArtifactIDs: map[string]string{}}
	verified := map[string]bool{}
	for _, check := range assessment.Checks {
		verified[check.Target] = check.Execution == "verified"
	}
	for _, o := range observed {
		if !verified[o.Target] {
			continue
		}
		m := map[string]benchmarkMetric{}
		for key := range d.spec.RequiredMetrics {
			m[key] = o.Metrics[key]
		}
		out.VerifiedMetrics[o.Target] = m
		if o.Raw != nil {
			out.RawArtifactIDs[o.Target] = o.Raw.Ref
		}
	}
	if out.Complete && len(d.spec.ComparisonKeys) > 0 {
		comparison, err := previousBenchmarkComparison(tx, d, out.VerifiedMetrics)
		if err != nil {
			return out, err
		}
		out.Comparison = comparison
	}

	return out, nil
}
