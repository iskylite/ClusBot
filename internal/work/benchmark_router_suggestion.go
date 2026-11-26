package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const benchmarkRouterEvent = "benchmark.admission_suggested"

type BenchmarkRouterSuggestion struct {
	Target          string              `json:"target"`
	Profile         BenchmarkProfileRef `json:"profile"`
	State           string              `json:"state"`
	BindingRevision int                 `json:"binding_revision,omitempty"`
}

type benchmarkRouterKey struct{}

func RouterBenchmarkSuggestion(ctx context.Context) *BenchmarkRouterSuggestion {
	v, _ := ctx.Value(benchmarkRouterKey{}).(*BenchmarkRouterSuggestion)
	return v
}

func benchmarkRouterSuggestionTx(ctx context.Context, tx *sql.Tx, actor, conversation, text string) (*BenchmarkRouterSuggestion, error) {
	s := strings.TrimSpace(text)
	if benchmarkAdmissionText(text) != nil || strings.ContainsAny(text, "\n\r`'") ||
		strings.HasPrefix(s, "[") || strings.HasPrefix(s, ">") || strings.HasPrefix(s, "$") ||
		strings.Contains(text, "日志") || strings.Contains(text, "示例") ||
		!strings.Contains(text, "基准") {
		return nil, nil
	}
	if allowed, err := ownsPersonalBusiness(tx, actor); err != nil || !allowed {
		return nil, err
	}
	if conversation != "" {
		var existing bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM tasks WHERE conversation_id=?)", conversation).Scan(&existing); err != nil {
			return nil, err
		}
		if existing {
			return nil, nil // ordinary follow-up, never suggest a new task
		}
	}
	tokens := benchmarkAdmissionTokens.FindAllString(strings.ToUpper(text), 65)
	if len(tokens) == 0 || len(tokens) > 64 {
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT t.target,t.profile_key,t.profile_version,r.revision FROM benchmark_chat_targets t
JOIN benchmark_chat_target_revisions r ON r.target=t.target
WHERE t.target IN (SELECT value FROM json_each(?)) ORDER BY t.target LIMIT 2`, toJSON(tokens))
	if err != nil {
		return nil, err
	}
	found := []BenchmarkRouterSuggestion{}
	for rows.Next() {
		var v BenchmarkRouterSuggestion
		if err := rows.Scan(&v.Target, &v.Profile.Profile, &v.Profile.Version, &v.BindingRevision); err != nil {
			rows.Close()
			return nil, err
		}
		found = append(found, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(found) != 1 {
		return nil, err
	}
	out := found[0]
	p, err := readBenchmarkProfile(tx, out.Profile.Profile, out.Profile.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	stamp := now()
	for _, c := range p.Cases {
		for _, ref := range c.Targets {
			if _, err := benchmarkAsset(tx, ref); errors.Is(err, ErrState) || errors.Is(err, ErrBenchmarkInput) {
				return nil, nil
			} else if err != nil {
				return nil, err
			}
			if _, err := benchmarkAuthorization(tx, actor, ref, c, stamp); errors.Is(err, ErrAuthorizationDenied) {
				return nil, nil
			} else if err != nil {
				return nil, err
			}
		}
	}
	out.State = "explicit_preview_required"
	return &out, nil
}

func benchmarkRouterActor(ctx context.Context, tx *sql.Tx, key string) (string, error) {
	var actor string
	err := tx.QueryRowContext(ctx, "SELECT principal FROM inbox_events WHERE source='chat' AND event_id=?", key).Scan(&actor)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return actor, err
}

func (s *Store) routerBenchmarkSuggestion(ctx context.Context, key, conversation, text string) (*BenchmarkRouterSuggestion, error) {
	if !strings.Contains(text, "基准") {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	actor, err := benchmarkRouterActor(ctx, tx, key)
	if err != nil {
		return nil, err
	}
	return benchmarkRouterSuggestionTx(ctx, tx, actor, conversation, text)
}

func readBenchmarkRouterReceipt(tx *sql.Tx, run string) (*BenchmarkRouterSuggestion, error) {
	var raw string
	err := tx.QueryRow("SELECT data FROM run_events WHERE run_id=? AND kind=? ORDER BY seq DESC LIMIT 1", run, benchmarkRouterEvent).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out BenchmarkRouterSuggestion
	err = json.Unmarshal([]byte(raw), &out)
	return &out, err
}

// Preserve the old fixed phrase as a no-model suggestion, not as a dispatch.
// Only the general conversational path calls this; explicit slash commands
// keep their previous contract.
func (s *Store) SubmitBenchmarkSuggestionShortcut(ctx context.Context, key, actor, conversation, text, scope string) (Run, bool, error) {
	target := benchmarkOrderTargetFromText(text)
	if target == "" {
		return Run{}, false, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, true, err
	}
	defer tx.Rollback()
	fingerprint := submissionFingerprint(text, scope, conversation, false)
	if previous, err := previousSubmission(tx, key, fingerprint); err != nil || previous.ID != "" {
		return previous, true, err
	}
	owner, err := benchmarkRouterActor(ctx, tx, key)
	if err != nil {
		return Run{}, true, err
	}
	if owner != "" {
		return Run{}, false, nil
	} // already admitted through normal routing
	if conversation != "" {
		var existing bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM tasks WHERE conversation_id=?)", conversation).Scan(&existing); err != nil {
			return Run{}, true, err
		}
		if existing {
			return Run{}, false, nil
		}
	}
	if _, err := admitChatInputWithFingerprint(tx, key, actor, text, fingerprint, "", now()); err != nil {
		return Run{}, true, err
	}
	c := submissionClassification{DelegationResult: DelegationResult{Route: &RouteDecision{Kind: "benchmark_preview", BenchmarkTarget: target}}}
	run, err := submitWorkTx(ctx, tx, key, fingerprint, text, scope, conversation, false, Delivery{}, &c, nil)
	if err != nil {
		return Run{}, true, err
	}
	if err := linkAdmissionToRun(tx, key, run.ID, "", now()); err != nil {
		return Run{}, true, err
	}
	return run, true, tx.Commit()
}

const benchmarkRouterSuggestionLifetime = 30 * time.Minute

type BenchmarkSuggestionPreviewInput struct {
	Action string `json:"action"`
}

// PreviewBenchmarkSuggestion reopens only a server-recorded Router decision.
// A repeat request revalidates authorization and returns the same preview,
// not a new model request, benchmark task or queue action.
func (s *Store) PreviewBenchmarkSuggestion(ctx context.Context, actor, conversation, run string, in BenchmarkSuggestionPreviewInput, provider BenchmarkProvider) (BenchmarkAdmissionPreview, error) {
	if in.Action != "preview_benchmark_suggestion" || conversation == "" || run == "" {
		return BenchmarkAdmissionPreview{}, ErrBenchmarkInput
	}
	if actor == "" {
		return BenchmarkAdmissionPreview{}, sql.ErrNoRows
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BenchmarkAdmissionPreview{}, err
	}
	defer tx.Rollback()
	source, err := readBenchmarkSuggestionSource(ctx, tx, actor, conversation, run)
	if err != nil {
		return BenchmarkAdmissionPreview{}, err
	}
	candidate, flight, err := resolveBenchmarkSuggestion(ctx, tx, actor, source, provider)
	if err != nil {
		return BenchmarkAdmissionPreview{}, err
	}
	out, err := saveBenchmarkSuggestionPreview(ctx, tx, actor, conversation, run, candidate, flight)
	if err != nil {
		return BenchmarkAdmissionPreview{}, err
	}
	return out, tx.Commit()
}

type benchmarkSuggestionSource struct {
	Text              string
	Created, Revision int64
	Archived          bool
	Receipt           *BenchmarkRouterSuggestion
}

func readBenchmarkSuggestionSource(ctx context.Context, tx *sql.Tx, actor, conversation, run string) (benchmarkSuggestionSource, error) {
	var source benchmarkSuggestionSource
	if err := tx.QueryRowContext(ctx, `SELECT m.text,r.created_at,c.revision,c.archived FROM runs r
JOIN conversations c ON c.id=r.conversation_id
JOIN messages m ON m.run_id=r.id AND m.conversation_id=r.conversation_id AND m.role='user'
WHERE r.id=? AND r.conversation_id=? AND r.status='completed' AND r.task_id IS NULL
AND EXISTS (SELECT 1 FROM inbox_events i JOIN submissions sub ON sub.request_key=i.event_id
WHERE i.source='chat' AND i.run_id=r.id AND i.state='applied' AND i.principal=? AND sub.run_id=r.id)
ORDER BY m.seq LIMIT 1`, run, conversation, actor).Scan(&source.Text, &source.Created, &source.Revision, &source.Archived); err != nil {
		return source, err
	}
	receipt, err := readBenchmarkRouterReceipt(tx, run)
	if err != nil {
		return source, err
	}
	if receipt == nil || receipt.State != "explicit_preview_required" {
		return source, sql.ErrNoRows
	}
	source.Receipt = receipt
	return source, nil
}

func resolveBenchmarkSuggestion(ctx context.Context, tx *sql.Tx, actor string, source benchmarkSuggestionSource, provider BenchmarkProvider) (BenchmarkAdmissionCandidate, BenchmarkPreflight, error) {
	stamp := now()
	if source.Created <= 0 || stamp < source.Created || stamp-source.Created >= benchmarkRouterSuggestionLifetime.Milliseconds() || benchmarkAdmissionText(source.Text) != nil {
		return BenchmarkAdmissionCandidate{}, BenchmarkPreflight{}, ErrBenchmarkAdmissionPreviewStale
	}
	candidate, flight, err := benchmarkAdmissionVisibleTx(ctx, tx, actor, source.Text, provider)
	if errors.Is(err, ErrBenchmarkAdmissionClarify) || errors.Is(err, ErrBenchmarkInput) || errors.Is(err, ErrState) || errors.Is(err, sql.ErrNoRows) {
		return BenchmarkAdmissionCandidate{}, BenchmarkPreflight{}, ErrBenchmarkAdmissionPreviewStale
	}
	if err != nil {
		return BenchmarkAdmissionCandidate{}, BenchmarkPreflight{}, err
	}
	if source.Receipt.BindingRevision < 1 || candidate.Target != source.Receipt.Target || candidate.Profile != source.Receipt.Profile ||
		candidate.bindingRevision != source.Receipt.BindingRevision {
		return BenchmarkAdmissionCandidate{}, BenchmarkPreflight{}, ErrBenchmarkAdmissionPreviewStale
	}
	return candidate, flight, nil
}

func benchmarkSuggestionPreviewID(actor, conversation, run string) string {
	return digest(toJSON([]string{"benchmark-suggestion-preview", actor, conversation, run}))[:32]
}

func saveBenchmarkSuggestionPreview(ctx context.Context, tx *sql.Tx, actor, conversation, run string, candidate BenchmarkAdmissionCandidate, flight BenchmarkPreflight) (BenchmarkAdmissionPreview, error) {
	// The deterministic ID belongs to this one click target. Confirm uses the
	// same general preview table and the same transactional admission guard.
	preview := benchmarkSuggestionPreviewID(actor, conversation, run)
	if _, err := tx.ExecContext(ctx, `INSERT INTO benchmark_admission_previews
(preview_id,actor,target,profile_key,profile_version,snapshot_digest,created_at,source_conversation,source_run) VALUES(?,?,?,?,?,?,?,?,?)
ON CONFLICT(preview_id) DO UPDATE SET source_conversation=excluded.source_conversation,source_run=excluded.source_run
WHERE task_id='' AND source_conversation='' AND source_run=''
 AND actor=excluded.actor AND target=excluded.target AND profile_key=excluded.profile_key AND profile_version=excluded.profile_version AND snapshot_digest=excluded.snapshot_digest`,
		preview, actor, candidate.Target, candidate.Profile.Profile,
		candidate.Profile.Version, candidate.snapshotDigest(), now(), conversation, run); err != nil {
		return BenchmarkAdmissionPreview{}, err
	}
	var valid bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM benchmark_admission_previews
WHERE preview_id=? AND actor=? AND target=? AND profile_key=? AND profile_version=? AND snapshot_digest=?
AND ((source_conversation=? AND source_run=?) OR (task_id<>'' AND source_conversation='' AND source_run='')))`,
		preview, actor, candidate.Target, candidate.Profile.Profile, candidate.Profile.Version, candidate.snapshotDigest(), conversation, run).Scan(&valid); err != nil {
		return BenchmarkAdmissionPreview{}, err
	}
	if !valid {
		return BenchmarkAdmissionPreview{}, ErrBenchmarkAdmissionPreviewStale
	}
	return BenchmarkAdmissionPreview{PreviewID: preview, State: "preview_only", Selection: candidate, Preflight: flight}, nil
}
