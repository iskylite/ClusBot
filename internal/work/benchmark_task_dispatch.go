package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// This task-level control only gates future benchmark dispatch. It does not
// change a case, cancel a provider job, or release a hold.
type BenchmarkTaskDispatchStatus struct {
	Paused            bool   `json:"paused"`
	Available         bool   `json:"available"`
	ControlVersion    int    `json:"control_version"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
}

type BenchmarkTaskDispatchInput struct {
	Action                 string `json:"action"`
	ExpectedPlanRevision   int    `json:"expected_plan_revision"`
	ExpectedControlVersion *int   `json:"expected_control_version"`
	RequestID              string `json:"client_request_id"`
}

type BenchmarkTaskDispatchReceipt struct {
	TaskID         string `json:"task_id"`
	Action         string `json:"action"`
	State          string `json:"state"`
	PlanRevision   int    `json:"plan_revision"`
	ControlVersion int    `json:"control_version"`
}

type benchmarkTaskDispatchRecord struct {
	Fingerprint string                       `json:"fingerprint"`
	Receipt     BenchmarkTaskDispatchReceipt `json:"receipt"`
}

func benchmarkTaskDispatchKey(actor, requestID string) string {
	return "benchmark-task-dispatch:" + digest(toJSON([]string{actor, requestID}))
}

func validBenchmarkTaskDispatchRequestID(requestID string) bool {
	return len(requestID) >= 8 && len(requestID) <= 128 && strings.TrimSpace(requestID) == requestID
}

// ReadBenchmarkTaskDispatchReceipt lets a client check an uncertain response
// without issuing a new control decision.
func (s *Store) ReadBenchmarkTaskDispatchReceipt(ctx context.Context, actor, task, requestID string) (BenchmarkTaskDispatchReceipt, error) {
	var receipt BenchmarkTaskDispatchReceipt
	if actor == "" || task == "" || !validBenchmarkTaskDispatchRequestID(requestID) {
		return receipt, ErrBenchmarkInput
	}
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT c.detail FROM control_records c
JOIN benchmark_runs b ON b.task_id=c.task_id
WHERE c.request_key=? AND c.task_id=? AND c.decided_by=? AND b.actor=?`,
		benchmarkTaskDispatchKey(actor, requestID), task, actor, actor).Scan(&raw)
	if err != nil {
		return receipt, err
	}
	var record benchmarkTaskDispatchRecord
	if err := json.Unmarshal([]byte(raw), &record); err != nil {
		return BenchmarkTaskDispatchReceipt{}, err
	}
	return record.Receipt, nil
}

// The gate transition and a scheduler claim share the same write transaction.
// Once a claim commits, its original operation is reconciled even if pause
// commits before the provider call begins.
func (s *Store) ControlBenchmarkTaskDispatch(ctx context.Context, actor, task string, in BenchmarkTaskDispatchInput, provider BenchmarkProvider) (BenchmarkTaskDispatchReceipt, error) {
	var out BenchmarkTaskDispatchReceipt
	if actor == "" || task == "" || (in.Action != "pause" && in.Action != "resume") ||
		in.ExpectedPlanRevision < 1 || in.ExpectedControlVersion == nil || *in.ExpectedControlVersion < 0 ||
		!validBenchmarkTaskDispatchRequestID(in.RequestID) {
		return out, ErrBenchmarkInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var plan int
	var stopping bool
	var ownerStatus string
	if err := tx.QueryRowContext(ctx, `SELECT b.plan_revision,b.stop_requested,t.status
FROM benchmark_runs b JOIN tasks t ON t.id=b.task_id WHERE b.task_id=? AND b.actor=?`, task, actor).Scan(&plan, &stopping, &ownerStatus); err != nil {
		return out, err
	}
	key := benchmarkTaskDispatchKey(actor, in.RequestID)
	fingerprint := digest(toJSON([]any{task, in.Action, in.ExpectedPlanRevision, *in.ExpectedControlVersion}))
	var stored string
	err = tx.QueryRowContext(ctx, `SELECT detail FROM control_records WHERE request_key=? AND task_id=? AND decided_by=?`, key, task, actor).Scan(&stored)
	if err == nil {
		var record benchmarkTaskDispatchRecord
		if err := json.Unmarshal([]byte(stored), &record); err != nil {
			return BenchmarkTaskDispatchReceipt{}, err
		}
		if fingerprint != record.Fingerprint {
			return out, ErrConflict
		}
		return record.Receipt, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	if pin := accountBenchmarkDispatchPin(provider); pin != "" {
		var admitted bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM benchmark_account_dispatch_admissions WHERE task_id=? AND provider_pin=?)", task, pin).Scan(&admitted); err != nil {
			return out, err
		}
		if !admitted {
			return out, fmt.Errorf("%w：当前提供方没有这项任务的派发授权", ErrBenchmarkBinding)
		}
	}
	current, err := readTaskState(tx, task)
	if err != nil {
		return out, err
	}
	if stopping || ownerStatus != "active" || plan != in.ExpectedPlanRevision || current.ControlRevision != *in.ExpectedControlVersion {
		return out, fmt.Errorf("%w：任务方案或控制状态已变化，请刷新后重试", ErrConflict)
	}
	if in.Action == "pause" && current.State != RuntimeTaskPending && current.State != RuntimeTaskRunning ||
		in.Action == "resume" && current.State != RuntimeTaskPaused {
		return out, fmt.Errorf("%w：当前状态不能执行该操作", ErrState)
	}
	stamp := now()
	if in.Action == "resume" {
		if err := recheckBenchmarkTaskDispatch(ctx, tx, actor, task, provider, stamp); err != nil {
			return out, err
		}
	}
	event := EventPause
	state, reason := "paused", "已暂停后续派发；已派发作业继续核对"
	if in.Action == "resume" {
		event, state, reason = EventResume, "resumed", "已恢复后续派发；执行前仍会核对条件"
	}
	if _, err := transitionTaskState(tx, task, event, TaskGuard{Actor: actor, ExpectedStorageRevision: current.StorageRevision}, stamp); err != nil {
		return out, err
	}
	// A resumed task with a pre-existing operation is still running. If all
	// cases settled during pause, enter running so it can finalize normally.
	if in.Action == "resume" {
		var queued, inFlight bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM benchmark_cases WHERE task_id=? AND phase='queued'),
EXISTS(SELECT 1 FROM benchmark_cases WHERE task_id=? AND phase IN ('submitting','following'))`, task, task).Scan(&queued, &inFlight); err != nil {
			return out, err
		}
		if inFlight || !queued {
			if _, err := transitionTaskState(tx, task, EventClaim, TaskGuard{Actor: "system", ExpectedStorageRevision: current.StorageRevision + 1}, stamp); err != nil {
				return out, err
			}
		}
	}
	out = BenchmarkTaskDispatchReceipt{TaskID: task, Action: in.Action, State: state, PlanRevision: plan, ControlVersion: current.ControlRevision + 1}
	if _, err := tx.Exec(`UPDATE task_states SET control_revision=? WHERE task_id=?`, out.ControlVersion, task); err != nil {
		return BenchmarkTaskDispatchReceipt{}, err
	}
	statusReason := "benchmark_dispatch_paused"
	if in.Action == "resume" {
		statusReason = "benchmark_dispatch_resumed"
	}
	if err := projectTaskFromRuntimeState(tx, task, "", statusReason, reason, stamp); err != nil {
		return BenchmarkTaskDispatchReceipt{}, err
	}
	decision := "已暂停后续派发"
	if in.Action == "resume" {
		decision = "已恢复后续派发"
	}
	if _, err := insertControlRecord(tx, ControlRecord{RecordID: id(), TaskID: task, RequestKey: key, DecidedBy: actor,
		Kind: in.Action, ControlRevision: out.ControlVersion, TargetContractRevision: plan,
		Decision: decision, Detail: toJSON(benchmarkTaskDispatchRecord{Fingerprint: fingerprint, Receipt: out}), CreatedAt: stamp}); err != nil {
		return BenchmarkTaskDispatchReceipt{}, err
	}
	if in.Action == "resume" {
		if err := finalizeBenchmarkIfSettled(tx, task, stamp); err != nil {
			return BenchmarkTaskDispatchReceipt{}, err
		}
	}
	return out, tx.Commit()
}

func recheckBenchmarkTaskDispatch(ctx context.Context, tx *sql.Tx, actor, task string, provider BenchmarkProvider, stamp int64) error {
	rows, err := tx.Query(`SELECT b.case_key,b.case_spec,b.frozen_targets,b.authorization_refs,b.operation_id,`+benchmarkHasBoundWorkSQL+`
FROM benchmark_cases b LEFT JOIN benchmark_dispatch_gates g ON g.step_id=b.step_id
WHERE b.task_id=? AND b.phase='queued' AND coalesce(g.held,0)=0`, task)
	if err != nil {
		return err
	}
	type pending struct {
		key, spec, targets, grants, operation string
		bound                                 bool
	}
	var cases []pending
	for rows.Next() {
		var c pending
		if err := rows.Scan(&c.key, &c.spec, &c.targets, &c.grants, &c.operation, &c.bound); err != nil {
			rows.Close()
			return err
		}
		cases = append(cases, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	var profile BenchmarkProfile
	var loaded bool
	for _, row := range cases {
		if row.operation != "" || row.bound {
			return fmt.Errorf("%w：待派发测项已有执行记录，请先核对", ErrState)
		}
		var c BenchmarkCase
		if row.spec != "" {
			if err := json.Unmarshal([]byte(row.spec), &c); err != nil {
				return err
			}
		} else {
			if !loaded {
				var key string
				var version int
				if err := tx.QueryRow("SELECT profile_key,profile_version FROM benchmark_runs WHERE task_id=?", task).Scan(&key, &version); err != nil {
					return err
				}
				profile, err = readBenchmarkProfile(tx, key, version)
				if err != nil {
					return err
				}
				loaded = true
			}
			for _, def := range profile.Cases {
				if def.Key == row.key {
					c = def
					break
				}
			}
		}
		if c.Key == "" || c.Key != row.key {
			return fmt.Errorf("%w：冻结的基准连接或测项不可用", ErrBenchmarkBinding)
		}
		selection, err := selectBenchmarkProvider(ctx, tx, provider, c.Binding)
		if err != nil {
			return err
		}
		if err := selection.validate(benchmarkCaseRequest(c, "benchmark:preflight")); err != nil {
			return err
		}
		var targets []ticketTarget
		var grants map[string]string
		if err := json.Unmarshal([]byte(row.targets), &targets); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(row.grants), &grants); err != nil {
			return err
		}
		if len(targets) == 0 || len(targets) != len(c.Targets) {
			return ErrState
		}
		for _, target := range targets {
			asset, err := benchmarkAsset(tx, target.Ref)
			if err != nil {
				return err
			}
			if !benchmarkControlAssetMatches(target, asset.Revision, asset.OwnershipEpoch) {
				return ErrAuthorizationDenied
			}
			if err := authorizeFrozenBenchmarkGrant(tx, actor, target.Ref, c, stamp, grants[target.Ref]); err != nil {
				return err
			}
		}
	}
	return nil
}
