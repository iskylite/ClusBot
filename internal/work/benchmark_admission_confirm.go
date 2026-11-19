package work

import (
	"context"
	"database/sql"
	"errors"
)

const benchmarkAdmissionConfirmPrefix = "admission-preview:"

var ErrBenchmarkAdmissionPreviewStale = errors.New("受理预览已失效，请按当前目标状态重新预览")

func migrateBenchmarkAdmissionPreviews(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS benchmark_admission_previews (
 preview_id TEXT PRIMARY KEY, actor TEXT NOT NULL, target TEXT NOT NULL, profile_key TEXT NOT NULL,
 profile_version INTEGER NOT NULL, snapshot_digest TEXT NOT NULL, created_at INTEGER NOT NULL,
 task_id TEXT NOT NULL DEFAULT '', confirmed_at INTEGER NOT NULL DEFAULT 0
)`)
	return err
}

type BenchmarkAdmissionConfirmationInput struct {
	Action string `json:"action"`
}

type BenchmarkAdmissionConfirmation struct {
	PreviewID string `json:"preview_id"`
	TaskID    string `json:"task_id"`
	State     string `json:"state"`
}

// ConfirmBenchmarkAdmission queues only the exact server-issued selection.
// It does not invoke the model or any external provider operation.
func (s *Store) ConfirmBenchmarkAdmission(ctx context.Context, actor, preview string, in BenchmarkAdmissionConfirmationInput, provider BenchmarkProvider) (BenchmarkAdmissionConfirmation, error) {
	if in.Action != "accept_benchmark_admission" || preview == "" {
		return BenchmarkAdmissionConfirmation{}, ErrBenchmarkInput
	}
	if actor == "" {
		return BenchmarkAdmissionConfirmation{}, sql.ErrNoRows
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BenchmarkAdmissionConfirmation{}, err
	}
	defer tx.Rollback()
	var c BenchmarkAdmissionCandidate
	var wanted, accepted string
	if err := tx.QueryRowContext(ctx, `SELECT target,profile_key,profile_version,snapshot_digest,task_id
FROM benchmark_admission_previews WHERE preview_id=? AND actor=?`, preview, actor).
		Scan(&c.Target, &c.Profile.Profile, &c.Profile.Version, &wanted, &accepted); err != nil {
		return BenchmarkAdmissionConfirmation{}, err
	}
	out := BenchmarkAdmissionConfirmation{PreviewID: preview, TaskID: accepted, State: "accepted"}
	if accepted != "" {
		return out, nil // historic, not a claim about the current task's state
	}
	var bound BenchmarkProfileRef
	err = tx.QueryRowContext(ctx, "SELECT profile_key,profile_version FROM benchmark_chat_targets WHERE target=?", c.Target).Scan(&bound.Profile, &bound.Version)
	if errors.Is(err, sql.ErrNoRows) || err == nil && bound != c.Profile {
		return BenchmarkAdmissionConfirmation{}, ErrBenchmarkAdmissionPreviewStale
	}
	if err != nil {
		return BenchmarkAdmissionConfirmation{}, err
	}
	current, _, err := benchmarkAdmissionCandidateTx(ctx, tx, actor, c, provider)
	switch {
	case errors.Is(err, ErrAuthorizationDenied):
		return BenchmarkAdmissionConfirmation{}, ErrAuthorizationDenied
	case errors.Is(err, ErrBenchmarkInput), errors.Is(err, ErrBenchmarkBinding), errors.Is(err, ErrState), errors.Is(err, sql.ErrNoRows):
		return BenchmarkAdmissionConfirmation{}, ErrBenchmarkAdmissionPreviewStale
	case err != nil:
		return BenchmarkAdmissionConfirmation{}, err
	case wanted != current.inventoryFingerprint:
		return BenchmarkAdmissionConfirmation{}, ErrBenchmarkAdmissionPreviewStale
	}
	stamp := now()
	out.TaskID, err = submitBenchmarkRunTx(tx, actor, BenchmarkRunRequest{
		RequestKey: benchmarkAdmissionConfirmPrefix + preview, Profile: c.Profile.Profile, Version: c.Profile.Version,
	}, provider, stamp)
	if err != nil {
		return BenchmarkAdmissionConfirmation{}, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE benchmark_admission_previews SET task_id=?,confirmed_at=?
WHERE preview_id=? AND actor=? AND task_id=''`, out.TaskID, stamp, preview, actor)
	if err != nil {
		return BenchmarkAdmissionConfirmation{}, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return BenchmarkAdmissionConfirmation{}, err
	} else if n != 1 {
		return BenchmarkAdmissionConfirmation{}, ErrConflict
	}
	if err := recordAudit(tx, AuditEvent{TaskID: out.TaskID, Event: "benchmark_admission_confirmed", Actor: actor,
		DetailRef: preview, Summary: "确认受理基准任务（未派发）", OccurredAt: stamp}); err != nil {
		return BenchmarkAdmissionConfirmation{}, err
	}
	return out, tx.Commit()
}
