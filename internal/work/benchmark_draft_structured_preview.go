package work

import (
	"context"
	"fmt"
)

// BenchmarkDraftStructuredPreview records both exact documents so callers can
// present dependency, metric/unit, raw-evidence and threshold changes without
// guessing from the changed case keys. It is not a save or approval receipt.
type BenchmarkDraftStructuredPreview struct {
	BenchmarkDraftEditPreview
	CurrentProfile BenchmarkProfile `json:"current_profile"`
}

// PreviewBenchmarkDraftStructuredEdit validates a caller-supplied complete
// candidate against the exact draft revision without egress or persistence.
// An invalid current draft may be repaired; the candidate must be valid.
func (s *Store) PreviewBenchmarkDraftStructuredEdit(ctx context.Context, actor, key string, req BenchmarkDraftRequest) (BenchmarkDraftStructuredPreview, error) {
	var empty BenchmarkDraftStructuredPreview
	if actor == "" {
		return empty, ErrAuthorizationDenied
	}
	if req.ExpectedRevision < 1 {
		return empty, ErrBenchmarkInput
	}
	proposed, _, err := encodeBenchmarkDraftRequest(key, req)
	if err != nil {
		return empty, err
	}
	current, err := s.readPinnedBenchmarkDraft(ctx, actor, key, req.BaseVersion, req.ExpectedRevision)
	if err != nil {
		return empty, err
	}
	if err := validateBenchmarkProfile(proposed); err != nil {
		return empty, err
	}
	changes := diffBenchmarkDraft(current.Profile.Cases, proposed.Cases)
	if len(changes.Added)+len(changes.Removed)+len(changes.Changed) == 0 && !changes.OrderChanged {
		return empty, fmt.Errorf("%w：草稿内容没有变化", ErrBenchmarkInput)
	}
	return BenchmarkDraftStructuredPreview{
		BenchmarkDraftEditPreview: BenchmarkDraftEditPreview{
			State: "preview_only", BaseVersion: current.BaseVersion, ExpectedRevision: current.Revision,
			ProposedProfile: proposed, Changes: changes,
		},
		CurrentProfile: current.Profile,
	}, nil
}
