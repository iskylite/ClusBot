package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

const benchmarkDraftSchema = `
CREATE TABLE IF NOT EXISTS benchmark_profile_drafts (
  profile_key TEXT NOT NULL,
  actor TEXT NOT NULL,
  revision INTEGER NOT NULL,
  base_version INTEGER NOT NULL,
  document TEXT NOT NULL,
  published INTEGER NOT NULL DEFAULT 0 CHECK (published IN (0,1)),
  PRIMARY KEY(profile_key,actor)
);
`

const benchmarkDraftMaxBytes = 1 << 20

func migrateBenchmarkDrafts(tx *sql.Tx) error {
	_, err := tx.Exec(benchmarkDraftSchema)
	return err
}

type BenchmarkDraftRequest struct {
	ExpectedRevision int             `json:"expected_revision"`
	BaseVersion      int             `json:"base_version"`
	Cases            []BenchmarkCase `json:"cases"`
}

type BenchmarkDraft struct {
	Profile         BenchmarkProfile    `json:"profile"`
	Revision        int                 `json:"revision"`
	BaseVersion     int                 `json:"base_version"`
	Published       bool                `json:"published"`
	ValidationError string              `json:"validation_error,omitempty"`
	Changes         BenchmarkDraftDelta `json:"changes"`
}

type BenchmarkDraftDelta struct {
	Added        []string `json:"added"`
	Removed      []string `json:"removed"`
	Changed      []string `json:"changed"`
	OrderChanged bool     `json:"order_changed"`
}

func loadBenchmarkDraft(ctx context.Context, tx *sql.Tx, actor, key string) (BenchmarkDraft, error) {
	var draft BenchmarkDraft
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT revision,base_version,document,published FROM benchmark_profile_drafts
WHERE profile_key=? AND actor=?`, key, actor).Scan(&draft.Revision, &draft.BaseVersion, &raw, &draft.Published)
	if err != nil {
		return draft, err
	}
	if err := json.Unmarshal([]byte(raw), &draft.Profile); err != nil {
		return draft, err
	}
	if err := validateBenchmarkProfile(draft.Profile); err != nil {
		draft.ValidationError = err.Error()
	}
	var base BenchmarkProfile
	if draft.BaseVersion > 0 {
		base, err = readBenchmarkProfile(tx, key, draft.BaseVersion)
		if err != nil {
			return draft, err
		}
	}
	draft.Changes = diffBenchmarkDraft(base.Cases, draft.Profile.Cases)
	return draft, nil
}

func diffBenchmarkDraft(base, next []BenchmarkCase) BenchmarkDraftDelta {
	d := BenchmarkDraftDelta{Added: []string{}, Removed: []string{}, Changed: []string{}}
	left, right := map[string]BenchmarkCase{}, map[string]BenchmarkCase{}
	for _, item := range base {
		left[item.Key] = item
	}
	for _, item := range next {
		right[item.Key] = item
	}
	before, after := []string{}, []string{}
	for _, item := range base {
		if _, ok := right[item.Key]; ok {
			before = append(before, item.Key)
		} else {
			d.Removed = append(d.Removed, item.Key)
		}
	}
	for _, item := range next {
		if old, ok := left[item.Key]; ok {
			after = append(after, item.Key)
			if toJSON(old) != toJSON(item) {
				d.Changed = append(d.Changed, item.Key)
			}
		} else {
			d.Added = append(d.Added, item.Key)
		}
	}
	d.OrderChanged = !slices.Equal(before, after)
	return d
}

func guardBenchmarkDraftBase(ctx context.Context, tx *sql.Tx, key string, base int) error {
	var latest int
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(version),0) FROM benchmark_profiles WHERE profile_key=?`, key).Scan(&latest); err != nil {
		return err
	}
	if latest != base {
		return fmt.Errorf("%w：方案已发布到版本 %d，请以当前版本重新编辑", ErrConflict, latest)
	}
	return nil
}

func (s *Store) ReadBenchmarkDraft(ctx context.Context, actor, key string) (BenchmarkDraft, error) {
	if actor == "" {
		return BenchmarkDraft{}, ErrAuthorizationDenied
	}
	if !validBenchKey(key) {
		return BenchmarkDraft{}, ErrBenchmarkInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BenchmarkDraft{}, err
	}
	defer tx.Rollback()
	return loadBenchmarkDraft(ctx, tx, actor, key)
}

// encodeBenchmarkDraftRequest applies the same document and revision limits
// to a structured preview and to a subsequent explicit save.
func encodeBenchmarkDraftRequest(key string, req BenchmarkDraftRequest) (BenchmarkProfile, []byte, error) {
	const maxInt = int(^uint(0) >> 1)
	if !validBenchKey(key) || req.ExpectedRevision < 0 || req.ExpectedRevision == maxInt || req.BaseVersion < 0 || req.BaseVersion == maxInt || len(req.Cases) > 64 {
		return BenchmarkProfile{}, nil, ErrBenchmarkInput
	}
	profile := BenchmarkProfile{Key: key, Version: req.BaseVersion + 1, Cases: req.Cases}
	raw, err := json.Marshal(profile)
	if err != nil {
		return BenchmarkProfile{}, nil, err
	}
	if len(raw) > benchmarkDraftMaxBytes {
		return BenchmarkProfile{}, nil, ErrBenchmarkInput
	}
	return profile, raw, nil
}

func (s *Store) SaveBenchmarkDraft(ctx context.Context, actor, key string, req BenchmarkDraftRequest) (BenchmarkDraft, error) {
	if actor == "" {
		return BenchmarkDraft{}, ErrAuthorizationDenied
	}
	_, raw, err := encodeBenchmarkDraftRequest(key, req)
	if err != nil {
		return BenchmarkDraft{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BenchmarkDraft{}, err
	}
	defer tx.Rollback()
	if err := guardBenchmarkDraftBase(ctx, tx, key, req.BaseVersion); err != nil {
		return BenchmarkDraft{}, err
	}
	var revision int
	err = tx.QueryRowContext(ctx, `SELECT revision FROM benchmark_profile_drafts WHERE profile_key=? AND actor=?`, key, actor).Scan(&revision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return BenchmarkDraft{}, err
	}
	if revision != req.ExpectedRevision {
		return BenchmarkDraft{}, fmt.Errorf("%w：草稿已更新到修订 %d，请刷新后编辑", ErrConflict, revision)
	}
	draft, err := writeBenchmarkDraft(ctx, tx, actor, key, revision, req.BaseVersion, raw)
	if err != nil {
		return BenchmarkDraft{}, err
	}
	return draft, tx.Commit()
}

// StartBenchmarkDraftFromPublished initializes authoring from an immutable
// published snapshot, never from a run or client-supplied copies. An editor
// must explicitly continue any unpublished draft rather than replace it.
func (s *Store) StartBenchmarkDraftFromPublished(ctx context.Context, actor, key string, base, expected int) (BenchmarkDraft, error) {
	if actor == "" {
		return BenchmarkDraft{}, ErrAuthorizationDenied
	}
	const maxInt = int(^uint(0) >> 1)
	if !validBenchKey(key) || base < 1 || base == maxInt || expected < 0 || expected == maxInt {
		return BenchmarkDraft{}, ErrBenchmarkInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BenchmarkDraft{}, err
	}
	defer tx.Rollback()
	profile, err := readBenchmarkProfile(tx, key, base)
	if err != nil {
		return BenchmarkDraft{}, err
	}
	if err := guardBenchmarkDraftBase(ctx, tx, key, base); err != nil {
		return BenchmarkDraft{}, err
	}
	var revision int
	var published bool
	err = tx.QueryRowContext(ctx, `SELECT revision,published FROM benchmark_profile_drafts WHERE profile_key=? AND actor=?`, key, actor).Scan(&revision, &published)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return BenchmarkDraft{}, err
	}
	if revision != expected {
		return BenchmarkDraft{}, fmt.Errorf("%w：草稿已更新到修订 %d，请刷新后编辑", ErrConflict, revision)
	}
	if revision != 0 && !published {
		return BenchmarkDraft{}, fmt.Errorf("%w：已有未发布草稿，请继续编辑现有草稿", ErrConflict)
	}
	profile.Version = base + 1
	raw, err := json.Marshal(profile)
	if err != nil {
		return BenchmarkDraft{}, err
	}
	if len(raw) > benchmarkDraftMaxBytes {
		return BenchmarkDraft{}, ErrBenchmarkInput
	}
	draft, err := writeBenchmarkDraft(ctx, tx, actor, key, revision, base, raw)
	if err != nil {
		return BenchmarkDraft{}, err
	}
	return draft, tx.Commit()
}

func writeBenchmarkDraft(ctx context.Context, tx *sql.Tx, actor, key string, revision, base int, raw []byte) (BenchmarkDraft, error) {
	if _, err := tx.ExecContext(ctx, `INSERT INTO benchmark_profile_drafts(profile_key,actor,revision,base_version,document)
 VALUES(?,?,?,?,?) ON CONFLICT(profile_key,actor) DO UPDATE SET revision=excluded.revision,base_version=excluded.base_version,
 document=excluded.document,published=0`, key, actor, revision+1, base, string(raw)); err != nil {
		return BenchmarkDraft{}, err
	}
	return loadBenchmarkDraft(ctx, tx, actor, key)
}

func (s *Store) PublishBenchmarkDraft(ctx context.Context, actor, key string, expected int) (BenchmarkDraft, error) {
	if actor == "" {
		return BenchmarkDraft{}, ErrAuthorizationDenied
	}
	if !validBenchKey(key) || expected < 1 {
		return BenchmarkDraft{}, ErrBenchmarkInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BenchmarkDraft{}, err
	}
	defer tx.Rollback()
	draft, err := loadBenchmarkDraft(ctx, tx, actor, key)
	if err != nil {
		return BenchmarkDraft{}, err
	}
	if draft.Revision != expected {
		return BenchmarkDraft{}, fmt.Errorf("%w：草稿已更新到修订 %d，请刷新后发布", ErrConflict, draft.Revision)
	}
	if draft.Published {
		return draft, nil
	}
	if err := guardBenchmarkDraftBase(ctx, tx, key, draft.BaseVersion); err != nil {
		return BenchmarkDraft{}, err
	}
	if err := publishBenchmarkProfileTx(ctx, tx, actor, draft.Profile, now()); err != nil {
		return BenchmarkDraft{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE benchmark_profile_drafts SET published=1 WHERE profile_key=? AND actor=?`,
		key, actor); err != nil {
		return BenchmarkDraft{}, err
	}
	draft.Published = true
	return draft, tx.Commit()
}
