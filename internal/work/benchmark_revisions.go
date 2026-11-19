package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const benchmarkRevisionsSchema = `
CREATE TABLE IF NOT EXISTS benchmark_run_revisions (
 task_id TEXT NOT NULL REFERENCES benchmark_runs(task_id) ON DELETE CASCADE,
 revision INTEGER NOT NULL, request_key TEXT NOT NULL, request_digest TEXT NOT NULL,
 document TEXT NOT NULL, created_at INTEGER NOT NULL,
 PRIMARY KEY(task_id,revision), UNIQUE(task_id,request_key)
);`

func migrateBenchmarkRevisions(tx *sql.Tx) error {
	if err := addColumnIfMissing(tx, "benchmark_runs", "plan_revision", "ALTER TABLE benchmark_runs ADD COLUMN plan_revision INTEGER NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	if err := addColumnIfMissing(tx, "benchmark_cases", "case_spec", "ALTER TABLE benchmark_cases ADD COLUMN case_spec TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	_, err := tx.Exec(benchmarkRevisionsSchema)
	return err
}

type BenchmarkRevisionInput struct {
	ExpectedRevision int             `json:"expected_revision"`
	RequestKey       string          `json:"request_key,omitempty"`
	Cases            []BenchmarkCase `json:"cases"`
}

type BenchmarkRevision struct {
	Revision    int                           `json:"revision"`
	Profile     BenchmarkProfile              `json:"profile"`
	Changes     BenchmarkDraftDelta           `json:"changes"`
	Frozen      []string                      `json:"frozen_cases"`
	AssetImpact *BenchmarkRevisionAssetImpact `json:"asset_impact,omitempty"`
}

type benchmarkRevisionCase struct {
	step, phase, operation string
	targets                []ticketTarget
}

func (s *Store) ReviseBenchmark(ctx context.Context, actor, task string, req BenchmarkRevisionInput, provider BenchmarkProvider, apply bool) (BenchmarkRevision, error) {
	return s.reviseBenchmark(ctx, actor, task, req, provider, apply, nil)
}

func (s *Store) reviseBenchmark(ctx context.Context, actor, task string, req BenchmarkRevisionInput, provider BenchmarkProvider, apply bool, confirmation *benchmarkRevisionConfirmation) (BenchmarkRevision, error) {
	if actor == "" {
		return BenchmarkRevision{}, ErrAuthorizationDenied
	}
	if confirmation == nil && strings.HasPrefix(req.RequestKey, benchmarkChatConfirmationPrefix) {
		return BenchmarkRevision{}, ErrBenchmarkInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BenchmarkRevision{}, err
	}
	defer tx.Rollback()
	if confirmation != nil {
		recorded, err := checkedRecordedBenchmarkPreview(tx, actor, confirmation.conversation, confirmation.run)
		if err != nil {
			return BenchmarkRevision{}, err
		}
		if confirmation.expected != recorded.Proposal.ExpectedRevision {
			return BenchmarkRevision{}, ErrBenchmarkRevisionPreviewStale
		}
		var archived bool
		if err := tx.QueryRowContext(ctx, "SELECT archived FROM conversations WHERE id=?", confirmation.conversation).Scan(&archived); err != nil {
			return BenchmarkRevision{}, err
		}
		if archived {
			return BenchmarkRevision{}, ErrConversationArchived
		}

		task = recorded.TaskID
		req = BenchmarkRevisionInput{ExpectedRevision: recorded.Proposal.ExpectedRevision,
			RequestKey: benchmarkChatConfirmationPrefix + confirmation.run, Cases: recorded.Proposal.Preview.Profile.Cases}
	}
	if req.ExpectedRevision < 1 || len(req.Cases) < 1 || len(req.Cases) > 64 ||
		(apply && (len(req.RequestKey) < 8 || len(req.RequestKey) > 128)) {
		return BenchmarkRevision{}, ErrBenchmarkInput
	}
	var active, base int
	var key, state string
	var stopping bool
	err = tx.QueryRowContext(ctx, `SELECT r.profile_key,r.profile_version,r.plan_revision,r.stop_requested,t.state
FROM benchmark_runs r JOIN task_states t ON t.task_id=r.task_id WHERE r.task_id=? AND r.actor=?`, task, actor).Scan(&key, &base, &active, &stopping, &state)
	if err != nil {
		return BenchmarkRevision{}, err
	}
	proposal := BenchmarkProfile{Key: key, Version: base, Cases: req.Cases}
	raw := toJSON(proposal)
	if len(raw) > benchmarkDraftMaxBytes {
		return BenchmarkRevision{}, ErrBenchmarkInput
	}
	fingerprint := digest(toJSON([]any{req.ExpectedRevision, raw}))
	if apply {
		var priorDigest, recorded string
		err := tx.QueryRowContext(ctx, `SELECT request_digest,document FROM benchmark_run_revisions WHERE task_id=? AND request_key=?`,
			task, req.RequestKey).Scan(&priorDigest, &recorded)
		if err == nil {
			if priorDigest != fingerprint {
				return BenchmarkRevision{}, ErrConflict
			}
			var prior BenchmarkRevision
			err = json.Unmarshal([]byte(recorded), &prior)
			return prior, err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return BenchmarkRevision{}, err
		}
	}
	if stopping || (state != "pending" && state != "running" && state != "paused") {
		return BenchmarkRevision{}, fmt.Errorf("%w：任务已停止或进入收尾，不能修订", ErrState)
	}
	if active != req.ExpectedRevision {
		return BenchmarkRevision{}, fmt.Errorf("%w：测试任务当前方案修订为 %d", ErrTaskRevision, active)
	}
	if err := validateBenchmarkProfile(proposal); err != nil {
		return BenchmarkRevision{}, err
	}
	original, err := readBenchmarkProfile(tx, key, base)
	if err != nil {
		return BenchmarkRevision{}, err
	}
	current := original
	if active > 1 {
		prior, err := benchmarkRevisionAt(ctx, tx, task, active)
		if err != nil {
			return BenchmarkRevision{}, err
		}
		current = prior.Profile
	}
	old, next := map[string]BenchmarkCase{}, map[string]BenchmarkCase{}
	scope := map[string]bool{}
	for _, c := range original.Cases {
		for _, target := range c.Targets {
			scope[target] = true
		}
	}
	for _, c := range current.Cases {
		old[c.Key] = c
	}
	for _, c := range proposal.Cases {
		for _, target := range c.Targets {
			if !scope[target] {
				return BenchmarkRevision{}, fmt.Errorf("%w：修订不能扩展到原任务范围之外的目标 %s", ErrAuthorizationDenied, target)
			}
		}
		next[c.Key] = c
	}
	rows, err := tx.QueryContext(ctx, "SELECT case_key,step_id,phase,operation_id,frozen_targets FROM benchmark_cases WHERE task_id=?", task)
	if err != nil {
		return BenchmarkRevision{}, err
	}
	existing := map[string]benchmarkRevisionCase{}
	knownTargets := map[string]ticketTarget{}
	for rows.Next() {
		var key string
		var c benchmarkRevisionCase
		var frozen string
		if err := rows.Scan(&key, &c.step, &c.phase, &c.operation, &frozen); err != nil {
			rows.Close()
			return BenchmarkRevision{}, err
		}
		var targets []ticketTarget
		if err := json.Unmarshal([]byte(frozen), &targets); err != nil {
			rows.Close()
			return BenchmarkRevision{}, err
		}
		for _, target := range targets {
			if last, ok := knownTargets[target.Ref]; ok && last != target {
				rows.Close()
				return BenchmarkRevision{}, fmt.Errorf("%w：目标 %s 的冻结版本不一致", ErrState, target.Ref)
			}
			knownTargets[target.Ref] = target
		}
		c.targets = targets
		existing[key] = c
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return BenchmarkRevision{}, err
	}
	if len(existing) != len(old) {
		return BenchmarkRevision{}, ErrState
	}
	out := BenchmarkRevision{Revision: active + 1, Profile: proposal, Changes: diffBenchmarkDraft(current.Cases, proposal.Cases), Frozen: []string{}}
	if len(out.Changes.Added)+len(out.Changes.Removed)+len(out.Changes.Changed) == 0 && !out.Changes.OrderChanged {
		return BenchmarkRevision{}, fmt.Errorf("%w：没有需要提交的方案变更", ErrBenchmarkInput)
	}
	for _, oldCase := range current.Cases {
		row, ok := existing[oldCase.Key]
		if !ok {
			return BenchmarkRevision{}, ErrState
		}
		if row.phase == "queued" && row.operation == "" {
			continue
		}
		nextCase, ok := next[oldCase.Key]
		if !ok || toJSON(nextCase) != toJSON(oldCase) {
			return BenchmarkRevision{}, fmt.Errorf("%w：测项 %s 已派发或已结束，只能按原定义对账", ErrConflict, oldCase.Key)
		}
		out.Frozen = append(out.Frozen, oldCase.Key)
	}
	// Check the entire proposed DAG. Even unchanged queued cases share the
	// current authorization check before accepting a new task revision.
	type readyData struct{ targets, grants string }
	checked := map[string]readyData{}
	proposedTargets := map[string][]ticketTarget{}
	stamp := now()
	for _, c := range proposal.Cases {
		if row, ok := existing[c.Key]; ok && (row.phase != "queued" || row.operation != "") {
			proposedTargets[c.Key] = row.targets
			continue
		}
		targets, grants, err := freezeBenchmarkCase(tx, actor, c, provider, stamp)
		if err != nil {
			return BenchmarkRevision{}, err
		}
		for _, target := range targets {
			if knownTargets[target.Ref] != target {
				return BenchmarkRevision{}, fmt.Errorf("%w：目标 %s 的身份或占用版本已改变，请重新受理", ErrConflict, target.Ref)
			}
		}
		checked[c.Key] = readyData{toJSON(targets), toJSON(grants)}
		proposedTargets[c.Key] = targets
	}
	if !apply {
		out.AssetImpact, err = readBenchmarkRevisionAssetImpact(ctx, tx, task, active, existing, proposedTargets, out.Changes)
		if err != nil {
			return BenchmarkRevision{}, err
		}
		return out, nil
	}
	ids := map[string]string{}
	for _, c := range current.Cases {
		row := existing[c.Key]
		ids[c.Key] = row.step
		if _, keep := next[c.Key]; !keep {
			if _, err := tx.ExecContext(ctx, "DELETE FROM steps WHERE step_id=?", row.step); err != nil {
				return BenchmarkRevision{}, err
			}
			delete(ids, c.Key)
		}
	}
	for i, c := range proposal.Cases {
		step := ids[c.Key]
		meta, queued := checked[c.Key]
		if step == "" {
			step = id()
			ids[c.Key] = step
			if err := insertStep(tx, Step{StepID: step, TaskID: task, ContractRevision: 1, DefinitionKey: c.Key,
				DefinitionVersion: base, State: V3StepWaiting, Position: i + 1, CreatedAt: stamp, UpdatedAt: stamp}); err != nil {
				return BenchmarkRevision{}, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO benchmark_cases(step_id,task_id,case_key,frozen_targets,authorization_refs,phase,case_spec)
VALUES(?,?,?,?,?,'queued',?)`, step, task, c.Key, meta.targets, meta.grants, toJSON(c)); err != nil {
				return BenchmarkRevision{}, err
			}
		} else if queued {
			if _, err := tx.ExecContext(ctx, `UPDATE benchmark_cases SET frozen_targets=?, authorization_refs=?, case_spec=?, next_at=0 WHERE step_id=?`,
				meta.targets, meta.grants, toJSON(c), step); err != nil {
				return BenchmarkRevision{}, err
			}
		}
		// Display order is not an execution identity; only queued dependency edges are rewritten.
		if _, err := tx.ExecContext(ctx, "UPDATE steps SET position=?,updated_at=? WHERE step_id=?", i+1, stamp, step); err != nil {
			return BenchmarkRevision{}, err
		}
		if queued {
			if _, err := tx.ExecContext(ctx, "DELETE FROM step_dependencies WHERE step_id=?", step); err != nil {
				return BenchmarkRevision{}, err
			}
		}
	}
	for _, c := range proposal.Cases {
		if _, queued := checked[c.Key]; !queued {
			continue
		}
		for _, dep := range c.DependsOn {
			if err := insertStepDependency(tx, ids[c.Key], ids[dep], 0); err != nil {
				return BenchmarkRevision{}, err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO benchmark_run_revisions VALUES(?,?,?,?,?,?)`,
		task, out.Revision, req.RequestKey, fingerprint, toJSON(out), stamp); err != nil {
		return BenchmarkRevision{}, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE benchmark_runs SET plan_revision=? WHERE task_id=?", out.Revision, task); err != nil {
		return BenchmarkRevision{}, err
	}
	var previewRun string
	if confirmation != nil {
		previewRun = confirmation.run
	}
	if err := recordAudit(tx, AuditEvent{TaskID: task, RunID: previewRun, Event: "benchmark_task_revised", Actor: actor,
		Summary: fmt.Sprintf("单次测试方案修订 %d", out.Revision), OccurredAt: stamp}); err != nil {
		return BenchmarkRevision{}, err
	}
	if err := finalizeBenchmarkIfSettled(tx, task, stamp); err != nil {
		return BenchmarkRevision{}, err
	}
	return out, tx.Commit()
}

func benchmarkRevisionAt(ctx context.Context, tx *sql.Tx, task string, revision int) (BenchmarkRevision, error) {
	var raw string
	var out BenchmarkRevision
	if err := tx.QueryRowContext(ctx, "SELECT document FROM benchmark_run_revisions WHERE task_id=? AND revision=?", task, revision).Scan(&raw); err != nil {
		return out, err
	}
	err := json.Unmarshal([]byte(raw), &out)
	return out, err
}

func (s *Store) ReadBenchmarkRevision(ctx context.Context, actor, task string, revision int) (BenchmarkRevision, error) {
	if actor == "" {
		return BenchmarkRevision{}, ErrAuthorizationDenied
	}
	if revision < 0 {
		return BenchmarkRevision{}, ErrBenchmarkInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BenchmarkRevision{}, err
	}
	defer tx.Rollback()
	var key string
	var version, current int
	if err := tx.QueryRowContext(ctx, "SELECT profile_key,profile_version,plan_revision FROM benchmark_runs WHERE task_id=? AND actor=?", task, actor).
		Scan(&key, &version, &current); err != nil {
		return BenchmarkRevision{}, err
	}
	if revision == 0 {
		revision = current
	}
	if revision == 1 {
		base, err := readBenchmarkProfile(tx, key, version)
		return BenchmarkRevision{Revision: 1, Profile: base, Changes: diffBenchmarkDraft(nil, base.Cases), Frozen: []string{}}, err
	}
	return benchmarkRevisionAt(ctx, tx, task, revision)
}
