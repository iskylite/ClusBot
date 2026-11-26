package work

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const BenchmarkAdmissionPreviewAction = "preview_benchmark_admission"

var benchmarkAdmissionActionPolicy = assistantReadReceiptPolicy{
	source: "a2ui-benchmark-admission", kind: BenchmarkAdmissionPreviewAction, limit: 1,
}

type BenchmarkAdmissionCard struct {
	Revision     int64
	Snapshot     string
	State        string
	Suggestion   BenchmarkRouterSuggestion
	Preview      *BenchmarkAdmissionPreview
	AcceptedTask string
	candidate    BenchmarkAdmissionCandidate
	preflight    BenchmarkPreflight
}

func (s *Store) ReadBenchmarkAdmissionCard(ctx context.Context, actor, conversation, run string, provider BenchmarkProvider) (BenchmarkAdmissionCard, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return BenchmarkAdmissionCard{}, err
	}
	defer tx.Rollback()
	return benchmarkAdmissionCardTx(ctx, tx, actor, conversation, run, provider)
}

func benchmarkAdmissionCardTx(ctx context.Context, tx *sql.Tx, actor, conversation, run string, provider BenchmarkProvider) (BenchmarkAdmissionCard, error) {
	var card BenchmarkAdmissionCard
	if allowed, err := ownsPersonalBusiness(tx, actor); err != nil || !allowed {
		if err == nil {
			err = sql.ErrNoRows
		}
		return card, err
	}
	source, err := readBenchmarkSuggestionSource(ctx, tx, actor, conversation, run)
	if err != nil {
		return card, err
	}
	card.Revision, card.Suggestion = source.Revision, *source.Receipt
	var saved, task, persistedSnapshot string
	err = tx.QueryRowContext(ctx, `SELECT p.preview_id,p.task_id,p.snapshot_digest
FROM benchmark_admission_previews p
WHERE p.actor=? AND p.source_conversation=? AND p.source_run=?`, actor, conversation, run).Scan(&saved, &task, &persistedSnapshot)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return card, err
	}
	if task != "" {
		card.State, card.AcceptedTask = "accepted", task
		return card, nil
	}
	if source.Archived {
		card.State = "unavailable"
		return card, nil
	}
	candidate, flight, err := resolveBenchmarkSuggestion(ctx, tx, actor, source, provider)
	if errors.Is(err, ErrBenchmarkAdmissionPreviewStale) || errors.Is(err, ErrBenchmarkBinding) {
		card.State = "stale"
		return card, nil
	}
	if err != nil {
		return card, err
	}
	card.Snapshot, card.candidate, card.preflight = candidate.snapshotDigest(), candidate, flight
	var state, scope, outcome string
	var received int64
	err = tx.QueryRowContext(ctx, `SELECT state,summary,payload_ref,received_at FROM inbox_events
WHERE source=? AND principal=? AND run_id=? AND source_version=?`, benchmarkAdmissionActionPolicy.source,
		actor, run, conversation).Scan(&state, &scope, &outcome, &received)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return card, err
	}
	if (state != "" && scope != card.Snapshot) || (saved != "" && persistedSnapshot != card.Snapshot) {
		card.State = "stale"
		return card, nil
	}
	switch state {
	case "applying":
		if now()-received >= (30 * time.Second).Milliseconds() {
			card.State = "not_completed"
		} else {
			card.State = "pending"
		}
	case "failed":
		card.State = "not_completed"
	case "applied":
		switch {
		case outcome == "clarify":
			card.State = "clarify"
		case outcome != "" && outcome == saved:
			card.State = "previewed"
			card.Preview = &BenchmarkAdmissionPreview{PreviewID: saved, State: "preview_only", Selection: candidate, Preflight: flight}
		default:
			return card, ErrState
		}
	default:
		card.State = "ready"
	}
	return card, nil
}

func (s *Store) BeginBenchmarkAdmissionAction(ctx context.Context, a AssistantReadCardAction, provider BenchmarkProvider) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	card, err := benchmarkAdmissionCardTx(ctx, tx, a.Actor, a.Conversation, a.Run, provider)
	if err != nil {
		return false, err
	}
	if card.Revision != a.Revision || card.Snapshot == "" || card.Snapshot != a.Target || card.State == "stale" {
		return false, ErrBenchmarkAdmissionPreviewStale
	}
	if card.State == "not_completed" {
		return false, ErrA2UIActionFailed
	}
	done, err := beginAssistantReadReceipt(ctx, tx, benchmarkAdmissionActionPolicy, a, card.State == "previewed" || card.State == "clarify")
	if err != nil {
		return false, err
	}
	return done, tx.Commit()
}

func (s *Store) FinishBenchmarkAdmissionAction(ctx context.Context, a AssistantReadCardAction, decision string, provider BenchmarkProvider) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	card, err := benchmarkAdmissionCardTx(ctx, tx, a.Actor, a.Conversation, a.Run, provider)
	if err != nil {
		return err
	}
	if card.Revision != a.Revision || card.Snapshot != a.Target || card.State != "pending" {
		return ErrBenchmarkAdmissionPreviewStale
	}
	// Only the exact frozen suggestion can be turned into a preview. The
	// model never calls the confirmation handler or names a target.
	var outcome string
	switch decision {
	case "preview":
		preview, err := saveBenchmarkSuggestionPreview(ctx, tx, a.Actor, a.Conversation, a.Run, card.candidate, card.preflight)
		if err != nil {
			return err
		}
		outcome = preview.PreviewID
	case "clarify":
		outcome = "clarify"
	default:
		return ErrBenchmarkAdmissionModel
	}
	if err := finishAssistantReadReceipt(ctx, tx, benchmarkAdmissionActionPolicy, a); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE inbox_events SET payload_ref=?
WHERE source=? AND event_id=? AND principal=? AND payload_digest=? AND state='applied'`,
		outcome, benchmarkAdmissionActionPolicy.source, a.key(), a.Actor, a.fingerprint()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) FailBenchmarkAdmissionAction(ctx context.Context, a AssistantReadCardAction) error {
	return s.failAssistantReadReceipt(ctx, benchmarkAdmissionActionPolicy, a)
}
