package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
)

type benchmarkProjectedControlTarget struct {
	BenchmarkCaseTarget
	CurrentRevision int `json:"current_revision"`
	CurrentEpoch    int `json:"current_epoch"`
}

type benchmarkActionProjection struct {
	block        benchmarkActionBlock
	version      int
	held         bool
	spec, grants string
	targets      []benchmarkProjectedControlTarget
}

type benchmarkProjectedGrant struct{ object, constraints string }

// projectBenchmarkCaseActions uses the same sqlite snapshot as BenchmarkProgress.
// Load pinned authorizations in one query, not once per target or per action.
func projectBenchmarkCaseActions(ctx context.Context, tx *sql.Tx, actor string, p *BenchmarkRunProgress, inputs []benchmarkActionProjection) error {
	grants := map[string]benchmarkProjectedGrant{}
	if slices.ContainsFunc(inputs, func(f benchmarkActionProjection) bool { return f.block.code == "" }) {
		stamp := now()
		rows, err := tx.QueryContext(ctx, `SELECT auth_id,object_ref,param_constraints FROM authorizations WHERE `+liveBenchmarkGrantSQL+`
AND auth_id IN (SELECT g.value FROM benchmark_cases b,json_each(CASE WHEN json_valid(b.authorization_refs) THEN b.authorization_refs ELSE '{}' END) g
WHERE b.task_id=? AND b.phase='queued' AND b.operation_id='')`, actor, stamp, stamp, p.TaskID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var grant benchmarkProjectedGrant
			if err := rows.Scan(&id, &grant.object, &grant.constraints); err != nil {
				return err
			}
			grants[id] = grant
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	var base BenchmarkProfile
	var baseRead bool
	var baseErr error
	for i, f := range inputs {
		c := &p.Cases[i]
		var authorization benchmarkActionBlock
		if f.block.code == "" {
			var def BenchmarkCase
			if f.spec != "" {
				if err := json.Unmarshal([]byte(f.spec), &def); err != nil {
					authorization = benchmarkActionInvalid
				}
			} else {
				if !baseRead {
					base, baseErr = readBenchmarkProfile(tx, p.Profile, p.Version)
					baseRead = true
				}
				if at := slices.IndexFunc(base.Cases, func(d BenchmarkCase) bool { return d.Key == c.Key }); baseErr == nil && at >= 0 {
					def = base.Cases[at]
				}
			}
			if authorization.code == "" {
				authorization = benchmarkProjectedAuthorizationBlock(c.Key, def, f, grants)
			}
		}
		c.Actions = map[string]BenchmarkActionAvailability{}
		for _, action := range []string{benchmarkHoldBeforeDispatch, benchmarkResumeBeforeDispatch, benchmarkSkipThisRun} {
			block := f.block
			if block.code == "" {
				block = benchmarkActionGateBlock(action, f.held)
			}
			if block.code == "" {
				block = authorization
			}
			c.Actions[action] = BenchmarkActionAvailability{Enabled: block.code == "", ReasonCode: block.code, Reason: block.reason,
				ExpectedPlanRevision: p.PlanRevision, ExpectedControlVersion: f.version}
		}
	}
	return nil
}

func benchmarkProjectedAuthorizationBlock(key string, def BenchmarkCase, facts benchmarkActionProjection, grants map[string]benchmarkProjectedGrant) benchmarkActionBlock {
	var pinned map[string]string
	if def.Key == "" || def.Key != key || len(facts.targets) == 0 || len(facts.targets) != len(def.Targets) ||
		json.Unmarshal([]byte(facts.grants), &pinned) != nil {
		return benchmarkActionInvalid
	}
	for _, target := range facts.targets {
		if target.CurrentRevision == 0 || !benchmarkControlAssetMatches(ticketTarget{Ref: target.Ref, Revision: target.FrozenRevision, Epoch: target.OwnershipEpoch},
			target.CurrentRevision, target.CurrentEpoch) {
			return benchmarkActionAssetChanged
		}
		grant, ok := grants[pinned[target.Ref]]
		if !ok || grant.object != target.Ref || !benchmarkGrantConstraintsMatch(grant.constraints, def) {
			return benchmarkActionGrantUnavailable
		}
	}
	return benchmarkActionBlock{}
}
