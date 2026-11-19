package work

import (
	"context"
	"database/sql"
	"maps"
	"slices"
)

// BenchmarkRevisionAssetImpact describes local snapshots only. Target refs
// can name groups and are not node counts or proof of GPU/remote capacity.
type BenchmarkRevisionAssetImpact struct {
	Source       string                             `json:"source"`
	BaseRevision int                                `json:"base_revision"`
	ScopeLimit   string                             `json:"scope_limit"`
	ScopeChanged bool                               `json:"scope_changed"`
	Baseline     BenchmarkAssetScope                `json:"baseline"`
	Proposed     BenchmarkAssetScope                `json:"proposed"`
	Cases        []BenchmarkRevisionCaseAssetImpact `json:"changed_cases"`
	ActualHolds  []BenchmarkRevisionCaseHolds       `json:"current_local_holds"`
}

type BenchmarkRevisionCaseAssetImpact struct {
	CaseKey    string   `json:"case_key"`
	Change     string   `json:"change"`
	BeforeRefs []string `json:"before_refs"`
	AfterRefs  []string `json:"after_refs"`
}

type BenchmarkRevisionCaseHolds struct {
	CaseKey    string   `json:"case_key"`
	TargetRefs []string `json:"target_refs"`
}

func readBenchmarkRevisionAssetImpact(ctx context.Context, tx *sql.Tx, task string, base int, existing map[string]benchmarkRevisionCase,
	proposed map[string][]ticketTarget, delta BenchmarkDraftDelta) (*BenchmarkRevisionAssetImpact, error) {
	before := make(map[string][]ticketTarget, len(existing))
	for key, c := range existing {
		before[key] = c.targets
	}
	toScope := func(cases map[string][]ticketTarget) BenchmarkAssetScope {
		seen := map[string]bool{}
		for _, targets := range cases {
			for _, target := range targets {
				seen[target.Ref] = true
			}
		}
		return BenchmarkAssetScope{Source: "frozen_targets", TargetCount: len(seen), TargetRefs: slices.Sorted(maps.Keys(seen))}
	}
	topBefore, topAfter := toScope(before), toScope(proposed)
	out := &BenchmarkRevisionAssetImpact{Source: "frozen_targets", BaseRevision: base, ScopeLimit: "within_original_task", ScopeChanged: !slices.Equal(topBefore.TargetRefs, topAfter.TargetRefs),
		Baseline: topBefore, Proposed: topAfter, Cases: []BenchmarkRevisionCaseAssetImpact{}, ActualHolds: []BenchmarkRevisionCaseHolds{}}
	listRefs := func(a []ticketTarget) []string {
		out := make([]string, 0, len(a))
		for _, t := range a {
			out = append(out, t.Ref)
		}
		return out
	}
	add := func(kind string, keys []string) {
		for _, key := range keys {
			out.Cases = append(out.Cases, BenchmarkRevisionCaseAssetImpact{key, kind, listRefs(before[key]), listRefs(proposed[key])})
		}
	}
	add("added", delta.Added)
	add("removed", delta.Removed)
	add("changed", delta.Changed)

	// Holds are a separate persisted runtime fact, never part of the
	// proposal. Neither provider status nor available capacity is inferred.
	rows, err := tx.QueryContext(ctx, `SELECT c.case_key,h.target_ref
FROM benchmark_target_holds h JOIN benchmark_cases c ON c.step_id=h.step_id
WHERE c.task_id=? ORDER BY c.case_key,h.target_ref`, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, ref string
		if err := rows.Scan(&key, &ref); err != nil {
			return nil, err
		}
		n := len(out.ActualHolds)
		if n == 0 || out.ActualHolds[n-1].CaseKey != key {
			out.ActualHolds = append(out.ActualHolds, BenchmarkRevisionCaseHolds{CaseKey: key})
			n++
		}
		out.ActualHolds[n-1].TargetRefs = append(out.ActualHolds[n-1].TargetRefs, ref)
	}
	return out, rows.Err()
}
