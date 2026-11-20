package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

type BenchmarkDraftDependencyInput struct {
	BaseVersion       int    `json:"base_version"`
	ExpectedRevision  int    `json:"expected_revision"`
	CaseKey           string `json:"case_key"`
	PrerequisiteKey   string `json:"prerequisite_key"`
	Text              string `json:"text"`
	ConfirmTextEgress bool   `json:"confirm_text_egress"`
}

type BenchmarkDraftDependencyPreview struct {
	BenchmarkDraftEditPreview
	Action          string `json:"action"`
	CaseKey         string `json:"case_key"`
	PrerequisiteKey string `json:"prerequisite_key"`
}

type BenchmarkDraftDependencyModelInput struct {
	Text            string `json:"text"`
	CaseKey         string `json:"case_key"`
	PrerequisiteKey string `json:"prerequisite_key"`
}

type BenchmarkDraftDependencyModel func(context.Context, BenchmarkDraftDependencyModelInput, int) (string, error)

func benchmarkDraftDependencyIndex(d BenchmarkDraft, key, prerequisite string) int {
	i := slices.IndexFunc(d.Profile.Cases, func(c BenchmarkCase) bool { return c.Key == key })
	if i < 0 || !slices.ContainsFunc(d.Profile.Cases, func(c BenchmarkCase) bool { return c.Key == prerequisite }) {
		return -1
	}
	return i
}

// PreviewBenchmarkDraftDependency does not persist a change. The two local
// case keys are selected by the caller, never returned by the model. The
// caller can apply the exact preview through the existing versioned SaveDraft.
func (s *Store) PreviewBenchmarkDraftDependency(ctx context.Context, actor, key string, in BenchmarkDraftDependencyInput,
	read BenchmarkDraftDependencyModel) (BenchmarkDraftDependencyPreview, error) {
	var empty BenchmarkDraftDependencyPreview
	if actor == "" {
		return empty, ErrAuthorizationDenied
	}
	if !validBenchKey(key) || in.BaseVersion < 0 || in.ExpectedRevision < 1 || !validBenchKey(in.CaseKey) ||
		!validBenchKey(in.PrerequisiteKey) || in.CaseKey == in.PrerequisiteKey || !in.ConfirmTextEgress ||
		strings.TrimSpace(in.Text) == "" || len(in.Text) > 4096 {
		return empty, ErrBenchmarkInput
	}
	load := func(ctx context.Context) (BenchmarkDraft, error) {
		return s.benchmarkDraftPreviewBase(ctx, actor, key, in.BaseVersion, in.ExpectedRevision)
	}
	before, err := load(ctx)
	if err != nil {
		return empty, err
	}
	if benchmarkDraftDependencyIndex(before, in.CaseKey, in.PrerequisiteKey) < 0 {
		return empty, fmt.Errorf("%w：请选择草稿里已有的测项与前置测项", ErrBenchmarkInput)
	}
	if read == nil {
		return empty, ErrBenchmarkRevisionModel
	}
	call, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	release, err := s.acquireModelSlot(call)
	if err != nil {
		return empty, ErrBenchmarkRevisionModel
	}
	raw, err := func() (string, error) {
		defer release()
		pinned, err := load(call)
		if err != nil {
			return "", err
		}
		if toJSON(pinned.Profile) != toJSON(before.Profile) {
			return "", ErrConflict
		}
		s.runtimeMu.Lock()
		budget := min(256, s.admissionLimits.OutputTokens)
		s.runtimeMu.Unlock()
		if budget < 32 {
			return "", ErrBenchmarkRevisionModel
		}
		return read(call, BenchmarkDraftDependencyModelInput{Text: in.Text, CaseKey: in.CaseKey, PrerequisiteKey: in.PrerequisiteKey}, budget)
	}()
	if err != nil {
		return empty, err
	}
	if call.Err() != nil {
		return empty, ErrBenchmarkRevisionModel
	}
	current, err := load(ctx)
	if err != nil {
		return empty, err
	}
	if toJSON(current.Profile) != toJSON(before.Profile) {
		return empty, ErrConflict
	}
	action, err := parseBenchmarkDraftDependency(raw, in.Text)
	if err != nil {
		return empty, err
	}
	index := benchmarkDraftDependencyIndex(current, in.CaseKey, in.PrerequisiteKey)
	proposal := current.Profile
	proposal.Cases = slices.Clone(proposal.Cases)
	c := &proposal.Cases[index]
	existing := slices.Index(c.DependsOn, in.PrerequisiteKey)
	if (existing >= 0) == (action == "add_dependency") {
		return empty, fmt.Errorf("%w：所选依赖没有变化", ErrBenchmarkInput)
	}
	c.DependsOn = slices.Clone(c.DependsOn)
	if action == "add_dependency" {
		c.DependsOn = append(c.DependsOn, in.PrerequisiteKey)
	} else {
		c.DependsOn = slices.Delete(c.DependsOn, existing, existing+1)
	}
	if err := validateBenchmarkProfile(proposal); err != nil {
		return empty, err
	}
	return BenchmarkDraftDependencyPreview{
		BenchmarkDraftEditPreview: BenchmarkDraftEditPreview{State: "preview_only", BaseVersion: current.BaseVersion,
			ExpectedRevision: current.Revision, ProposedProfile: proposal, Changes: diffBenchmarkDraft(current.Profile.Cases, proposal.Cases)},
		Action: action, CaseKey: in.CaseKey, PrerequisiteKey: in.PrerequisiteKey,
	}, nil
}

func parseBenchmarkDraftDependency(raw, text string) (string, error) {
	if len(raw) > 4096 {
		return "", ErrBenchmarkRevisionModel
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	if open, err := dec.Token(); err != nil || open != json.Delim('{') {
		return "", ErrBenchmarkRevisionModel
	}
	var action, quote *string
	for dec.More() {
		switch key, err := dec.Token(); {
		case err != nil:
			return "", ErrBenchmarkRevisionModel
		case key == "action" && action == nil:
			if dec.Decode(&action) != nil || action == nil {
				return "", ErrBenchmarkRevisionModel
			}
		case key == "quote" && quote == nil:
			if dec.Decode(&quote) != nil || quote == nil {
				return "", ErrBenchmarkRevisionModel
			}
		default:
			return "", ErrBenchmarkRevisionModel
		}
	}
	if close, err := dec.Token(); err != nil || close != json.Delim('}') {
		return "", ErrBenchmarkRevisionModel
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) || action == nil {
		return "", ErrBenchmarkRevisionModel
	}
	if *action == "clarify" && quote == nil {
		return "", fmt.Errorf("%w：请明确要求增加还是移除所选测项的依赖", ErrBenchmarkInput)
	}
	if (*action != "add_dependency" && *action != "remove_dependency") || quote == nil ||
		*quote == "" || strings.TrimSpace(*quote) != *quote || !strings.Contains(text, *quote) {
		return "", ErrBenchmarkRevisionModel
	}
	return *action, nil
}
