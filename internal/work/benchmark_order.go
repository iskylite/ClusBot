package work

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// A target name is an operator-maintained binding to one immutable full
// benchmark profile, not a wildcard and not a model-generated inventory query.
const benchmarkOrderSchema = `
CREATE TABLE IF NOT EXISTS benchmark_chat_targets (
  target TEXT PRIMARY KEY,
  profile_key TEXT NOT NULL,
  profile_version INTEGER NOT NULL,
  FOREIGN KEY(profile_key,profile_version) REFERENCES benchmark_profiles(profile_key,version)
);
`

func migrateBenchmarkOrders(tx *sql.Tx) error {
	_, err := tx.Exec(benchmarkOrderSchema)
	return err
}

const benchmarkOrderTargetPattern = "[A-Za-z0-9][A-Za-z0-9_-]{0,63}"

var benchmarkOrderTarget = regexp.MustCompile("^" + benchmarkOrderTargetPattern + "$")

// Accept an entire direct instruction only. Questions, quoted examples,
// multiple operations and extra parameters must not be truncated into orders.
var benchmarkOrderPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^(?:请[ \t]*(?:帮我[ \t]*)?|帮我[ \t]*)?(?:跑|执行)(?:一下|一遍)?[ \t]*(` + benchmarkOrderTargetPattern + `)[ \t]*(?:的[ \t]*)?(?:全套|完整(?:的)?)[ \t]*基准(?:测试)?[。！!.]?$`),
	regexp.MustCompile(`^(?:请[ \t]*(?:帮我[ \t]*)?|帮我[ \t]*)?在[ \t]*(` + benchmarkOrderTargetPattern + `)[ \t]*(?:上[ \t]*)?(?:跑|执行)(?:一下|一遍)?[ \t]*(?:全套|完整(?:的)?)[ \t]*基准(?:测试)?[。！!.]?$`),
}

func benchmarkOrderTargetFromText(text string) string {
	for _, pattern := range benchmarkOrderPatterns {
		if m := pattern.FindStringSubmatch(strings.TrimSpace(text)); len(m) == 2 {
			return strings.ToUpper(m[1])
		}
	}
	return ""
}

// SetBenchmarkChatTarget sets (or, with nil, removes) the default full profile
// for a target. A newly published profile never activates itself. Switching a
// target affects new requests only; previously accepted requests keep their
// frozen profile. This is operator configuration, not a grant to run tests.
func (s *Store) SetBenchmarkChatTarget(ctx context.Context, actor, target string, selector *BenchmarkProfileRef) error {
	return s.UpdateBenchmarkChatTarget(ctx, actor, target, selector, nil)
}

func (s *Store) UpdateBenchmarkChatTarget(ctx context.Context, actor, target string, selector *BenchmarkProfileRef, expected *int) error {
	if actor == "" {
		return ErrAuthorizationDenied
	}
	if !benchmarkOrderTarget.MatchString(target) {
		return fmt.Errorf("%w：目标名须由字母、数字、连字符或下划线组成", ErrBenchmarkInput)
	}
	target = strings.ToUpper(target)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	state, err := readBenchmarkChatTargetState(ctx, tx, target)
	if err != nil {
		return err
	}
	if expected != nil && *expected != state.BindingRevision {
		return ErrBenchmarkChatTargetConflict
	}
	if (selector == nil && state.State == "disabled") ||
		(selector != nil && state.State == "bound" && *selector == state.BenchmarkProfileRef) {
		return nil
	}
	if expected == nil && state.BindingRevision != 0 {
		return ErrBenchmarkChatTargetConflict
	}
	action := "benchmark_chat_target_disabled"
	summary := target
	if selector == nil {
		if _, err := tx.ExecContext(ctx, "DELETE FROM benchmark_chat_targets WHERE target=?", target); err != nil {
			return err
		}
	} else {
		if !validBenchKey(selector.Profile) || selector.Version < 1 {
			return ErrBenchmarkInput
		}
		if _, err := readBenchmarkProfile(tx, selector.Profile, selector.Version); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w：指定版本尚未发布", ErrBenchmarkInput)
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO benchmark_chat_targets(target,profile_key,profile_version) VALUES(?,?,?)
ON CONFLICT(target) DO UPDATE SET profile_key=excluded.profile_key,profile_version=excluded.profile_version`,
			target, selector.Profile, selector.Version); err != nil {
			return err
		}
		action = "benchmark_chat_target_set"
		summary = fmt.Sprintf("%s -> %s@%d", target, selector.Profile, selector.Version)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO benchmark_chat_target_revisions(target,revision) VALUES(?,1)
ON CONFLICT(target) DO UPDATE SET revision=revision+1 WHERE revision=?`, target, state.BindingRevision)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return ErrBenchmarkChatTargetConflict
	}
	if err := recordAudit(tx, AuditEvent{Event: action, Actor: actor, Summary: summary, OccurredAt: now()}); err != nil {
		return err
	}
	return tx.Commit()
}

type BenchmarkProfileRef struct {
	Profile string `json:"profile"`
	Version int    `json:"version"`
}
