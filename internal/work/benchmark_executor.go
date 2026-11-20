package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	spec                                      BenchmarkCase
	step, task, actor, operation, job, action string
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
		confirmed, err = provider.Start(call, d.request)
	case "lookup":
		confirmed, err = provider.Lookup(call, d.request)
	case "get":
		result, err = provider.Get(call, d.request, d.job)
	case "cancel":
		err = provider.(benchmarkCanceller).Cancel(call, d.request, d.job)
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
	var profile, phase, targetsRaw, authsRaw, caseSpec string
	var version int
	var stopping, cancelSent bool
	// Zero means immediately eligible for a queued case, not oldest work in
	// the database. Sort by its persisted enqueue/revision and prerequisite
	// settlement times so new submissions cannot overtake due reconciliation.
	err = tx.QueryRow(`SELECT b.step_id,b.task_id,r.actor,r.profile_key,r.profile_version,b.phase,b.operation_id,b.job_id,b.generation,
b.case_key,b.frozen_targets,b.authorization_refs,r.stop_requested,b.cancel_sent,b.case_spec FROM benchmark_cases b
JOIN benchmark_runs r ON r.task_id=b.task_id JOIN tasks t ON t.id=b.task_id
JOIN task_states ts ON ts.task_id=b.task_id JOIN steps s ON s.step_id=b.step_id
WHERE b.next_at<=? AND (b.phase IN ('submitting','following') OR
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
  r.created_at, s.position, b.step_id LIMIT 1`, cutoff).
		Scan(&d.step, &d.task, &d.actor, &profile, &version, &phase, &d.operation, &d.job, &d.generation, &d.request.Case, &targetsRaw, &authsRaw, &stopping, &cancelSent, &caseSpec)
	if errors.Is(err, sql.ErrNoRows) {
		return d, false, nil
	}
	if err != nil {
		return d, false, err
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
	d.request.Binding, d.request.Targets, d.request.Parameters = d.spec.Binding, d.spec.Targets, d.spec.Parameters
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
		if !provider.Supports(d.request.Binding) {
			reason = "冻结的基准连接或工具不可用"
		}
		var targets []ticketTarget
		var auths map[string]string
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
			aid, e := benchmarkAuthorization(tx, d.actor, target.Ref, d.spec, stamp)
			if e != nil && !errors.Is(e, ErrAuthorizationDenied) {
				return d, false, e
			}
			if e != nil || aid != auths[target.Ref] {
				reason = "冻结的执行授权已失效"
				break
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
		current, err := readTaskState(tx, d.task)
		if err != nil {
			return d, false, err
		}
		if current.State == V3TaskPending {
			if _, err = transitionTaskState(tx, d.task, EventClaim, TaskGuard{Actor: "system", ExpectedStorageRevision: current.StorageRevision}, stamp); err != nil {
				return d, false, err
			}
		}
		d.request.IdempotencyKey = "benchmark:" + d.step
		o, err := registerOperation(tx, Operation{TaskID: d.task, StepID: d.step, CapabilityKey: "benchmark." + d.spec.Binding.Tool,
			CapabilityVersion: d.spec.Binding.Revision, ConnectionID: d.spec.Binding.Connection, TargetRefs: targetsRaw,
			ParamsDigest: digest(toJSON(d.request)), AuthorizationRef: digest(authsRaw), IdempotencyKey: d.request.IdempotencyKey}, stamp)
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
	} else {
		if !provider.Supports(d.request.Binding) {
			_, err = tx.Exec("UPDATE benchmark_cases SET next_at=? WHERE step_id=?", stamp+30000, d.step)
			if err != nil {
				return d, false, err
			}
			return d, true, tx.Commit()
		}
		d.request.IdempotencyKey = "benchmark:" + d.step
		d.action = "lookup"
		if phase == "following" {
			d.action = "get"
			if _, supported := provider.(benchmarkCanceller); stopping && !cancelSent && supported {
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
		if _, err = tx.Exec(`INSERT INTO benchmark_job_identities VALUES(?,?,?,?)
ON CONFLICT(connection_key,binding_revision,provider_job_id) DO NOTHING`, d.spec.Binding.Connection, d.spec.Binding.Revision, confirmed, d.operation); err != nil {
			return err
		}
		var owner string
		if err = tx.QueryRow(`SELECT operation_id FROM benchmark_job_identities WHERE connection_key=? AND binding_revision=? AND provider_job_id=?`,
			d.spec.Binding.Connection, d.spec.Binding.Revision, confirmed).Scan(&owner); err != nil {
			return err
		}
		unknown = owner != d.operation
	}

	if unknown {
		if op.State == V3OperationDispatched || op.State == V3OperationRunning {
			if _, err = transitionOperation(tx, d.operation, V3OperationReconcile, stamp); err != nil {
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
		if op.State == V3OperationDispatched {
			if _, err = transitionOperation(tx, d.operation, V3OperationRunning, stamp); err != nil {
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
	to, jobState := V3OperationSucceeded, "succeeded"
	switch result.State {
	case "failed":
		to, jobState = V3OperationFailed, "failed"
	case "cancelled":
		to, jobState = V3OperationCancelled, "failed"
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
	stepState := V3StepFailed
	if phase == "verified" {
		stepState = V3StepCompleted
	} else if phase == "skipped" {
		stepState = V3StepCancelled
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
	if cur.State.IsTerminal() || (cur.State == V3TaskPaused && !stopping) {
		return nil
	}
	transition := func(event TaskEvent, result V3TaskState) error {
		_, e := transitionTaskState(tx, task, event, TaskGuard{ExpectedStorageRevision: cur.StorageRevision, Actor: "system", FinalState: result, AcceptanceEvidence: true}, stamp)
		cur.StorageRevision++
		return e
	}
	final, legacyTo, reason := V3TaskSucceeded, "completed", "已核实全部本次测量；性能达标单独记载"
	if legacy == "cancelled" || stopping {
		final, legacyTo, reason = V3TaskCancelled, "cancelled", "已停止；在途作业已完成对账"
		if cur.State != V3TaskFinalizing {
			if err = transition(EventCancel, ""); err != nil {
				return err
			}
		}
	} else if bad || revision != 1 {
		final, legacyTo, reason = V3TaskFailed, "interrupted", "存在缺失、未核实或未执行的必测项"
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
	_, err = tx.Exec(`UPDATE tasks SET status=?,status_reason='benchmark_result',reason=?,completed_at=?,revision=revision+1,updated_at=? WHERE id=?`, legacyTo, reason, stamp, stamp, task)
	return err
}

type v31BenchReport struct {
	benchmarkAssessment
	VerifiedMetrics map[string]map[string]benchmarkMetric `json:"verified_metrics"`
	RawArtifactIDs  map[string]string                     `json:"raw_artifact_ids,omitempty"`
	Comparison      *benchmarkComparison                  `json:"comparison,omitempty"`
}

func verifyBenchmarkJob(tx *sql.Tx, d benchmarkDispatch, r BenchmarkJobResult) (v31BenchReport, error) {
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
		return v31BenchReport{}, fmt.Errorf("%w：远端结果超出边界", ErrBenchmarkInput)
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
					return v31BenchReport{}, err
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
		return v31BenchReport{}, err
	}
	out := v31BenchReport{benchmarkAssessment: assessment, VerifiedMetrics: map[string]map[string]benchmarkMetric{}, RawArtifactIDs: map[string]string{}}
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
