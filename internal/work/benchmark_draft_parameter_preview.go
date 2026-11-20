package work

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

type BenchmarkDraftParameterInput struct {
	BaseVersion               int    `json:"base_version"`
	ExpectedRevision          int    `json:"expected_revision"`
	CaseKey                   string `json:"case_key"`
	Parameter                 string `json:"parameter"`
	Text                      string `json:"text"`
	ConfirmTextEgress         bool   `json:"confirm_text_egress"`
	AllowExpectedActualChange bool   `json:"allow_expected_actual_change"`
}

type BenchmarkDraftEditPreview struct {
	State            string              `json:"state"`
	BaseVersion      int                 `json:"base_version"`
	ExpectedRevision int                 `json:"expected_revision"`
	ProposedProfile  BenchmarkProfile    `json:"proposed_profile"`
	Changes          BenchmarkDraftDelta `json:"changes"`
}

type BenchmarkDraftParameterPreview = BenchmarkDraftEditPreview

type BenchmarkDraftParameterModelInput struct {
	Text      string `json:"text"`
	CaseKey   string `json:"case_key"`
	Parameter string `json:"parameter"`
}

type BenchmarkDraftParameterModel func(context.Context, BenchmarkDraftParameterModelInput, int) (string, error)

func (s *Store) readPinnedBenchmarkDraft(ctx context.Context, actor, key string, base, revision int) (BenchmarkDraft, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BenchmarkDraft{}, err
	}
	defer tx.Rollback()
	draft, err := loadBenchmarkDraft(ctx, tx, actor, key)
	if err != nil {
		return BenchmarkDraft{}, err
	}
	if draft.Published || draft.Revision != revision || draft.BaseVersion != base {
		return BenchmarkDraft{}, fmt.Errorf("%w：未发布草稿或基线版本已变化，请刷新后重新预演", ErrConflict)
	}
	if err := guardBenchmarkDraftBase(ctx, tx, key, base); err != nil {
		return BenchmarkDraft{}, err
	}
	return draft, nil
}

func (s *Store) benchmarkDraftPreviewBase(ctx context.Context, actor, key string, base, revision int) (BenchmarkDraft, error) {
	draft, err := s.readPinnedBenchmarkDraft(ctx, actor, key, base, revision)
	if err != nil {
		return BenchmarkDraft{}, err
	}
	if draft.ValidationError != "" {
		return BenchmarkDraft{}, fmt.Errorf("%w：请先修复草稿已有校验问题", ErrBenchmarkInput)
	}
	return draft, nil
}

func (s *Store) benchmarkDraftParameterBase(ctx context.Context, actor, key string, in BenchmarkDraftParameterInput) (BenchmarkDraft, int, error) {
	draft, err := s.benchmarkDraftPreviewBase(ctx, actor, key, in.BaseVersion, in.ExpectedRevision)
	if err != nil {
		return BenchmarkDraft{}, 0, err
	}
	for i, c := range draft.Profile.Cases {
		if c.Key != in.CaseKey {
			continue
		}
		if v := c.Parameters[in.Parameter]; v != "" && c.ActualConfig[in.Parameter] == v {
			return draft, i, nil
		}
	}
	return BenchmarkDraft{}, 0, fmt.Errorf("%w：请选择该草稿中已有的参数及对应实测期望", ErrBenchmarkInput)
}

// PreviewBenchmarkDraftParameter changes only a copy of the authorized
// current draft. It cannot choose which case or field to edit, and the model
// never receives current values or the draft document.
func (s *Store) PreviewBenchmarkDraftParameter(ctx context.Context, actor, key string, in BenchmarkDraftParameterInput,
	read BenchmarkDraftParameterModel) (BenchmarkDraftParameterPreview, error) {
	if actor == "" {
		return BenchmarkDraftParameterPreview{}, ErrAuthorizationDenied
	}
	if !validBenchKey(key) || in.BaseVersion < 1 || in.ExpectedRevision < 1 || !validBenchKey(in.CaseKey) || !validBenchKey(in.Parameter) ||
		strings.TrimSpace(in.Text) == "" || len(in.Text) > 4096 || !in.ConfirmTextEgress || !in.AllowExpectedActualChange {
		return BenchmarkDraftParameterPreview{}, ErrBenchmarkInput
	}
	before, _, err := s.benchmarkDraftParameterBase(ctx, actor, key, in)
	if err != nil {
		return BenchmarkDraftParameterPreview{}, err
	}
	if read == nil {
		return BenchmarkDraftParameterPreview{}, ErrBenchmarkRevisionModel
	}
	call, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	release, err := s.acquireModelSlot(call)
	if err != nil {
		return BenchmarkDraftParameterPreview{}, ErrBenchmarkRevisionModel
	}
	raw, err := func() (string, error) {
		defer release()
		// The prompt might have waited for shared model capacity. Recheck the exact version before egress.
		pinned, _, err := s.benchmarkDraftParameterBase(call, actor, key, in)
		if err != nil {
			return "", err
		}
		if toJSON(pinned.Profile) != toJSON(before.Profile) {
			return "", ErrConflict
		}
		s.runtimeMu.Lock()
		budget := min(512, s.admissionLimits.OutputTokens)
		s.runtimeMu.Unlock()
		if budget < 32 {
			return "", ErrBenchmarkRevisionModel
		}
		return read(call, BenchmarkDraftParameterModelInput{Text: in.Text, CaseKey: in.CaseKey, Parameter: in.Parameter}, budget)
	}()
	if err != nil {
		return BenchmarkDraftParameterPreview{}, err
	}
	if call.Err() != nil {
		return BenchmarkDraftParameterPreview{}, ErrBenchmarkRevisionModel
	}
	// Prefer the authoritative version result, even if a stale response's
	// proposed value would also fail validation.
	current, index, err := s.benchmarkDraftParameterBase(ctx, actor, key, in)
	if err != nil {
		return BenchmarkDraftParameterPreview{}, err
	}
	if toJSON(current.Profile) != toJSON(before.Profile) {
		return BenchmarkDraftParameterPreview{}, ErrConflict
	}
	intent, err := parseBenchmarkRevisionModelOutput(raw, BenchmarkRevisionAIInput{
		BenchmarkRevisionProposalInput: BenchmarkRevisionProposalInput{Text: in.Text}, AllowExpectedActualChange: true,
	})
	if err != nil {
		return BenchmarkDraftParameterPreview{}, err
	}
	if intent.Action != "set_parameter" || intent.CaseKey != in.CaseKey || intent.Parameter != in.Parameter || !intent.ChangeActual {
		return BenchmarkDraftParameterPreview{}, fmt.Errorf("%w：只支持预演已选测项的参数及对应实测期望", ErrBenchmarkInput)
	}
	proposal := current.Profile
	proposal.Cases = slices.Clone(proposal.Cases)
	c := &proposal.Cases[index]
	if c.Parameters[in.Parameter] == intent.Value {
		return BenchmarkDraftParameterPreview{}, fmt.Errorf("%w：所选参数没有变化", ErrBenchmarkInput)
	}
	c.Parameters = maps.Clone(c.Parameters)
	c.ActualConfig = maps.Clone(c.ActualConfig)
	c.Parameters[in.Parameter], c.ActualConfig[in.Parameter] = intent.Value, intent.Value
	if err := validateBenchmarkProfile(proposal); err != nil {
		return BenchmarkDraftParameterPreview{}, err
	}
	return BenchmarkDraftParameterPreview{State: "preview_only", BaseVersion: current.BaseVersion, ExpectedRevision: current.Revision,
		ProposedProfile: proposal, Changes: diffBenchmarkDraft(current.Profile.Cases, proposal.Cases)}, nil
}
