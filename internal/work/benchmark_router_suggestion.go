package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

const benchmarkRouterEvent = "benchmark.admission_suggested"

type BenchmarkRouterSuggestion struct {
	Target  string              `json:"target"`
	Profile BenchmarkProfileRef `json:"profile"`
	State   string              `json:"state"`
}

type benchmarkRouterKey struct{}

func RouterBenchmarkSuggestion(ctx context.Context) *BenchmarkRouterSuggestion {
	v, _ := ctx.Value(benchmarkRouterKey{}).(*BenchmarkRouterSuggestion)
	return v
}

func benchmarkRouterSuggestionTx(ctx context.Context, tx *sql.Tx, actor, conversation, text string) (*BenchmarkRouterSuggestion, error) {
	s := strings.TrimSpace(text)
	if actor != "user:session" || benchmarkAdmissionText(text) != nil || strings.ContainsAny(text, "\n\r`'") ||
		strings.HasPrefix(s, "[") || strings.HasPrefix(s, ">") || strings.HasPrefix(s, "$") ||
		strings.Contains(text, "日志") || strings.Contains(text, "示例") ||
		!strings.Contains(text, "基准") {
		return nil, nil
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
	rows, err := tx.QueryContext(ctx, `SELECT target,profile_key,profile_version FROM benchmark_chat_targets
WHERE target IN (SELECT value FROM json_each(?)) ORDER BY target LIMIT 2`, toJSON(tokens))
	if err != nil {
		return nil, err
	}
	found := []BenchmarkRouterSuggestion{}
	for rows.Next() {
		var v BenchmarkRouterSuggestion
		if err := rows.Scan(&v.Target, &v.Profile.Profile, &v.Profile.Version); err != nil {
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
