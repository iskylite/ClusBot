package work

import (
	"context"
	"database/sql"
	"errors"
)

const benchmarkAdmissionConfirmPrefix = "admission-preview:"
const benchmarkAdmissionQueuedEvent = "benchmark.admission_queued"

func migrateBenchmarkAdmissionConversationSource(tx *sql.Tx) error {
	if err := addColumnIfMissing(tx, "benchmark_admission_previews", "source_conversation",
		`ALTER TABLE benchmark_admission_previews ADD COLUMN source_conversation TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := addColumnIfMissing(tx, "benchmark_admission_previews", "source_run",
		`ALTER TABLE benchmark_admission_previews ADD COLUMN source_run TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	_, err := tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS benchmark_admission_preview_sources
ON benchmark_admission_previews(actor,source_conversation,source_run) WHERE source_run<>''`)
	return err
}

type benchmarkAdmissionQueuedReceipt struct {
	TaskID string `json:"task_id"`
}

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
	var wanted, accepted, conversation, sourceRun string
	if err := tx.QueryRowContext(ctx, `SELECT target,profile_key,profile_version,snapshot_digest,task_id,source_conversation,source_run
FROM benchmark_admission_previews WHERE preview_id=? AND actor=?`, preview, actor).
		Scan(&c.Target, &c.Profile.Profile, &c.Profile.Version, &wanted, &accepted, &conversation, &sourceRun); err != nil {
		return BenchmarkAdmissionConfirmation{}, err
	}
	out := BenchmarkAdmissionConfirmation{PreviewID: preview, TaskID: accepted, State: "accepted"}
	if accepted != "" {
		return out, nil // historic, not a claim about the current task's state
	}
	bound, err := readBenchmarkChatTargetState(ctx, tx, c.Target)
	if err != nil {
		return BenchmarkAdmissionConfirmation{}, err
	}
	if bound.State != "bound" || bound.BenchmarkProfileRef != c.Profile || bound.BindingRevision < 1 {
		return BenchmarkAdmissionConfirmation{}, ErrBenchmarkAdmissionPreviewStale
	}
	c.bindingRevision = bound.BindingRevision

	current, _, err := benchmarkAdmissionCandidateTx(ctx, tx, actor, c, provider)
	switch {
	case errors.Is(err, ErrAuthorizationDenied):
		return BenchmarkAdmissionConfirmation{}, ErrAuthorizationDenied
	case errors.Is(err, ErrBenchmarkInput), errors.Is(err, ErrBenchmarkBinding), errors.Is(err, ErrState), errors.Is(err, sql.ErrNoRows):
		return BenchmarkAdmissionConfirmation{}, ErrBenchmarkAdmissionPreviewStale
	case err != nil:
		return BenchmarkAdmissionConfirmation{}, err
	case wanted != current.snapshotDigest():
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
	if conversation != "" && sourceRun != "" {
		// A preview is independent of conversation deletion, but deleted
		// or detached source records must not produce a conversation event.
		var bound bool
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM runs r
WHERE r.id=? AND r.conversation_id=? AND r.task_id IS NULL AND r.status='completed'
AND EXISTS(SELECT 1 FROM run_events e WHERE e.run_id=r.id AND e.kind=?)
AND EXISTS(SELECT 1 FROM inbox_events i JOIN submissions sub ON sub.request_key=i.event_id
 WHERE i.source='chat' AND i.run_id=r.id AND i.state='applied' AND i.principal=? AND sub.run_id=r.id))`,
			sourceRun, conversation, benchmarkRouterEvent, actor).Scan(&bound)
		if err != nil {
			return BenchmarkAdmissionConfirmation{}, err
		}
		if bound {
			if err := addEvent(tx, sourceRun, benchmarkAdmissionQueuedEvent, benchmarkAdmissionQueuedReceipt{TaskID: out.TaskID}); err != nil {
				return BenchmarkAdmissionConfirmation{}, err
			}
		}
	}
	return out, tx.Commit()
}
