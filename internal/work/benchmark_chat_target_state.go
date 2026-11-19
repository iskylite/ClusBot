package work

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

var ErrBenchmarkChatTargetConflict = errors.New("目标默认方案已变化或请求缺少当前绑定版本，请刷新后重试")

type BenchmarkChatTargetState struct {
	BenchmarkProfileRef
	Target          string `json:"target"`
	State           string `json:"state"`
	BindingRevision int    `json:"binding_revision"`
}

func migrateBenchmarkChatTargetRevisions(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS benchmark_chat_target_revisions (
 target TEXT PRIMARY KEY,
 revision INTEGER NOT NULL CHECK(revision>0)
);
INSERT OR IGNORE INTO benchmark_chat_target_revisions SELECT target,1 FROM benchmark_chat_targets`)
	return err
}

func readBenchmarkChatTargetState(ctx context.Context, tx *sql.Tx, target string) (BenchmarkChatTargetState, error) {
	out := BenchmarkChatTargetState{Target: target, State: "disabled"}
	var profile sql.NullString
	var version sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT t.profile_key,t.profile_version,coalesce(r.revision,0)
FROM (SELECT ? AS key) a LEFT JOIN benchmark_chat_targets t ON t.target=a.key
LEFT JOIN benchmark_chat_target_revisions r ON r.target=a.key`, target).Scan(&profile, &version, &out.BindingRevision)
	if err == nil && profile.Valid && version.Valid {
		out.State, out.Profile, out.Version = "bound", profile.String, int(version.Int64)
	}
	return out, err
}

func (s *Store) ReadBenchmarkChatTargetState(ctx context.Context, actor, target string) (BenchmarkChatTargetState, error) {
	if actor == "" {
		return BenchmarkChatTargetState{}, ErrAuthorizationDenied
	}
	if !benchmarkOrderTarget.MatchString(target) {
		return BenchmarkChatTargetState{}, ErrBenchmarkInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BenchmarkChatTargetState{}, err
	}
	defer tx.Rollback()
	return readBenchmarkChatTargetState(ctx, tx, strings.ToUpper(target))
}
