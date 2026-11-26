package work

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

const assistantBenchmarkComparisonEvent = "assistant.benchmark_comparison_requested"

type AssistantBenchmarkComparisonRequest struct {
	Baseline, Current, Case string
}

func (r AssistantBenchmarkComparisonRequest) valid() bool {
	taskID := func(v string) bool {
		_, err := hex.DecodeString(v)
		return len(v) == 32 && strings.ToLower(v) == v && err == nil
	}
	return taskID(r.Baseline) && taskID(r.Current) && r.Baseline != r.Current && validBenchKey(r.Case)
}

type AssistantBenchmarkComparison struct {
	Revision                      int64
	Case, Reason, Proof           string
	BaselineStatus, CurrentStatus string
	Rows                          []AssistantBenchmarkMetricPair
	More                          bool
}

type AssistantBenchmarkMetricPair struct {
	Target, Metric    string
	Baseline, Current *BenchmarkMeasuredMetric
	Delta             *float64
}

type assistantBenchmarkSource struct {
	Profile, Phase, Step, State, Operation, Job, Connection, Capability, Pin string
	ProfileVersion, PlanRevision, CapabilityVersion                          int
	Targets                                                                  []ticketTarget
	Spec                                                                     BenchmarkCase
	Report                                                                   benchReportJSON
	Raw                                                                      string
	JobBound                                                                 bool
	Status                                                                   string
	MethodVerified                                                           bool
}

func (s *Store) OpenAssistantBenchmarkComparison(ctx context.Context, actor, conversation, requestKey, text string,
	req AssistantBenchmarkComparisonRequest) (Run, error) {
	if !req.valid() {
		return Run{}, ErrBenchmarkInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	if allowed, err := ownsPersonalBusiness(tx, actor); err != nil || !allowed {
		if err == nil {
			err = sql.ErrNoRows
		}
		return Run{}, err
	}
	if conversation != "" {
		if err := discoveryA2UIConversation(ctx, tx, actor, conversation); err != nil {
			return Run{}, err
		}
	}
	// Recheck both current read scopes before writing even a chat reference.
	// There is no benchmark task, job, operation, provider call or evidence write.
	if _, err := projectAssistantBenchmarkComparison(ctx, tx, actor, req); err != nil {
		return Run{}, err
	}
	key := "assistant-benchmark-compare:" + digest(toJSON([]string{actor, requestKey}))
	fingerprint := digest(toJSON([]any{assistantBenchmarkComparisonEvent, conversation, text, req}))
	if prior, err := previousSubmission(tx, key, fingerprint); err != nil || prior.ID != "" {
		return prior, err
	}
	intent := submissionClassification{DelegationResult: DelegationResult{Route: &RouteDecision{Kind: "clarify",
		Question: "可以核对两份已有基准报告的同一个测项。条件不同就只并列，不计算差值。"}}}
	run, err := submitWorkTxWithMode(ctx, tx, key, fingerprint, text, "", conversation, false, Delivery{}, &intent, nil, true)
	if err != nil {
		return Run{}, err
	}
	if err := addEvent(tx, run.ID, assistantBenchmarkComparisonEvent, req); err != nil {
		return Run{}, err
	}
	return run, tx.Commit()
}

func (s *Store) ReadAssistantBenchmarkComparison(ctx context.Context, actor, conversation, run string) (AssistantBenchmarkComparison, error) {
	var out AssistantBenchmarkComparison
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if err := discoveryA2UIConversation(ctx, tx, actor, conversation); err != nil {
		return out, err
	}
	var revision int64
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT c.revision,e.data FROM conversations c JOIN runs r ON r.conversation_id=c.id
JOIN run_events e ON e.run_id=r.id AND e.kind=? WHERE c.id=? AND r.id=?`, assistantBenchmarkComparisonEvent, conversation, run).Scan(&revision, &raw); err != nil {
		return out, err
	}
	var request AssistantBenchmarkComparisonRequest
	if json.Unmarshal([]byte(raw), &request) != nil || !request.valid() {
		return out, ErrState
	}
	out, err = projectAssistantBenchmarkComparison(ctx, tx, actor, request)
	out.Revision = revision
	return out, err
}

func readAssistantBenchmarkSource(ctx context.Context, tx *sql.Tx, actor, task, check string) (assistantBenchmarkSource, error) {
	var d assistantBenchmarkSource
	var targets, caseSpec, opTargets string
	err := tx.QueryRowContext(ctx, `SELECT r.profile_key,r.profile_version,r.plan_revision,c.phase,c.step_id,c.report,c.frozen_targets,c.case_spec,
coalesce(o.operation_id,''),coalesce(o.state,''),coalesce(o.connection_id,''),coalesce(o.capability_key,''),
coalesce(o.capability_version,0),coalesce(o.target_refs,''),c.job_id,coalesce(a.provider_pin,''),
EXISTS(SELECT 1 FROM operation_jobs j WHERE j.operation_id=o.operation_id AND j.provider_job_id=c.job_id AND j.query_key=o.idempotency_key AND j.state='succeeded')
FROM benchmark_runs r JOIN benchmark_cases c ON c.task_id=r.task_id
LEFT JOIN operations o ON o.operation_id=c.operation_id AND o.task_id=r.task_id AND o.step_id=c.step_id
LEFT JOIN benchmark_account_dispatch_admissions a ON a.task_id=r.task_id
WHERE r.actor=? AND r.task_id=? AND c.case_key=?`, actor, task, check).Scan(&d.Profile, &d.ProfileVersion, &d.PlanRevision, &d.Phase, &d.Step, &d.Raw,
		&targets, &caseSpec, &d.Operation, &d.State, &d.Connection, &d.Capability, &d.CapabilityVersion, &opTargets, &d.Job, &d.Pin, &d.JobBound)
	if err != nil {
		return d, err
	}
	if json.Unmarshal([]byte(targets), &d.Targets) != nil || len(d.Targets) < 1 || len(d.Targets) > 1024 {
		return d, ErrState
	}
	frozenOrder := make([]string, len(d.Targets))
	for i, t := range d.Targets {
		frozenOrder[i] = t.Ref
	}
	slices.SortFunc(d.Targets, func(a, b ticketTarget) int { return strings.Compare(a.Ref, b.Ref) })
	for i, target := range d.Targets {
		if target.Ref == "" || target.Revision < 1 || target.Epoch < 1 || i > 0 && d.Targets[i-1].Ref == target.Ref {
			return d, ErrState
		}
	}
	nowAt := now()
	var covered int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM json_each(?) frozen JOIN ops_assets a
ON a.object_ref=frozen.value->>'ref' AND a.ownership_epoch=frozen.value->>'epoch'
WHERE `+assistantAssetReadGrant, targets, actor, nowAt, nowAt).Scan(&covered); err != nil {
		return d, err
	}
	if covered != len(d.Targets) {
		// A grant for the object in its *new* ownership epoch does not
		// grant access to work done when the original data belonged elsewhere.
		return d, sql.ErrNoRows
	}
	d.Status = "unverified"
	if d.Phase != "verified" || d.State != string(RuntimeOperationSucceeded) || !d.JobBound ||
		d.Operation == "" || d.Job == "" || d.Connection == "" || d.Capability == "" || d.CapabilityVersion < 1 || opTargets != targets {
		return d, nil
	}

	profile, err := readBenchmarkProfile(tx, d.Profile, d.ProfileVersion)
	if err != nil {
		return d, err
	}
	at := slices.IndexFunc(profile.Cases, func(c BenchmarkCase) bool { return c.Key == check })
	if at >= 0 {
		d.Spec = profile.Cases[at]
	}
	if caseSpec != "" {
		var actual BenchmarkCase
		if json.Unmarshal([]byte(caseSpec), &actual) != nil || actual.Key != check {
			return d, ErrState
		}
		d.Spec = actual
	}
	// The verifier checked every declared actual condition, not just launch
	// arguments. Untouched published versions are the only supported method
	// baseline; revised plans never imply equivalence here.
	if d.Spec.Key != check {
		return d, nil
	}
	d.MethodVerified = d.PlanRevision == 1 && at >= 0 && toJSON(d.Spec) == toJSON(profile.Cases[at]) &&
		len(d.Spec.ComparisonKeys) > 0 && slices.Equal(d.Spec.Targets, frozenOrder)
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM benchmark_job_identities
WHERE connection_key=? AND binding_revision=? AND provider_job_id=? AND operation_id=?)`,
		d.Connection, d.Spec.Binding.Revision, d.Job, d.Operation).Scan(&d.JobBound); err != nil {
		return d, err
	}
	if !d.JobBound || len(d.Spec.Targets) != len(d.Targets) || json.Unmarshal([]byte(d.Raw), &d.Report) != nil ||
		!d.Report.Complete || d.Report.Verified != len(d.Targets) || len(d.Report.Checks) != len(d.Targets) ||
		len(d.Report.VerifiedMetrics) != len(d.Targets) {
		return d, nil
	}
	verified := make(map[string]bool, len(d.Targets))
	for _, c := range d.Report.Checks {
		if c.Check != check || c.Execution != "verified" || verified[c.Target] {
			return d, nil
		}
		verified[c.Target] = true
	}
	for _, target := range d.Targets {
		metrics := d.Report.VerifiedMetrics[target.Ref]
		if !verified[target.Ref] || !slices.Contains(d.Spec.Targets, target.Ref) || len(d.Spec.RequiredMetrics) == 0 || len(metrics) != len(d.Spec.RequiredMetrics) {
			return d, nil
		}
		for key, unit := range d.Spec.RequiredMetrics {
			metric, ok := metrics[key]
			if !ok || unit == "" || metric.Unit != unit || !finiteBenchmark(metric.Value) {
				d.Status = "unit_or_value_unverified"
				return d, nil
			}
		}
		if d.Spec.NeedsRawEvidence {
			artifactID := d.Report.RawArtifactIDs[target.Ref]
			var present bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM benchmark_evidence
WHERE artifact_id=? AND step_id=? AND target_ref=? AND length(body)>0 AND length(sha256)=64)`,
				artifactID, d.Step, target.Ref).Scan(&present); err != nil {
				return d, err
			}
			if artifactID == "" || !present {
				d.Status = "missing_required_evidence"
				return d, nil
			}
		}
	}
	d.Status = "verified"
	return d, nil
}

func projectAssistantBenchmarkComparison(ctx context.Context, tx *sql.Tx, actor string, req AssistantBenchmarkComparisonRequest) (AssistantBenchmarkComparison, error) {
	var out AssistantBenchmarkComparison
	baseline, err := readAssistantBenchmarkSource(ctx, tx, actor, req.Baseline, req.Case)
	if err != nil {
		return out, err
	}
	current, err := readAssistantBenchmarkSource(ctx, tx, actor, req.Current, req.Case)
	if err != nil {
		return out, err
	}
	out.Case, out.BaselineStatus, out.CurrentStatus = req.Case, baseline.Status, current.Status
	bothVerified := baseline.Status == "verified" && current.Status == "verified"
	switch {
	case !bothVerified:
		out.Reason = "unverified"
	case !slices.Equal(baseline.Targets, current.Targets):
		out.Reason = "target_scope"
	case toJSON(baseline.Spec.RequiredMetrics) != toJSON(current.Spec.RequiredMetrics):
		out.Reason = "metric_or_unit"
	case !baseline.MethodVerified || !current.MethodVerified || baseline.Profile != current.Profile ||
		baseline.ProfileVersion != current.ProfileVersion || baseline.Connection != current.Connection ||
		baseline.Capability != current.Capability || baseline.CapabilityVersion != current.CapabilityVersion ||
		baseline.Pin != current.Pin || baseline.Report.Unexpected != 0 || current.Report.Unexpected != 0:
		out.Reason = "method_unverified"
	default:
		out.Reason = "comparable"
	}
	// Sign only freshly read facts so stale reports cannot be selected across a
	// model round trip. This transient digest is not an evidence hash or export.
	type metricKey struct{ target, metric string }
	pairs := map[metricKey]AssistantBenchmarkMetricPair{}
	add := func(d assistantBenchmarkSource, older bool) {
		if d.Status != "verified" {
			return
		}
		for target, metrics := range d.Report.VerifiedMetrics {
			for metric, m := range metrics {
				key := metricKey{target, metric}
				row := pairs[key]
				row.Target, row.Metric = target, metric
				readout := BenchmarkMeasuredMetric(m)
				if older {
					row.Baseline = &readout
				} else {
					row.Current = &readout
				}
				pairs[key] = row
			}
		}
	}
	add(baseline, true)
	add(current, false)
	keys := make([]metricKey, 0, len(pairs))
	for key := range pairs {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b metricKey) int {
		if order := strings.Compare(a.target, b.target); order != 0 {
			return order
		}
		return strings.Compare(a.metric, b.metric)
	})
	if out.Reason == "comparable" {
		for _, key := range keys {
			row := pairs[key]
			if row.Baseline == nil || row.Current == nil || row.Baseline.Unit != row.Current.Unit ||
				!finiteBenchmark(row.Current.Value-row.Baseline.Value) {
				out.Reason = "metric_or_unit"
				break
			}
		}
	}
	const limit = 8
	for i, key := range keys {
		if i == limit {
			out.More = true
			break
		}
		row := pairs[key]
		if out.Reason == "comparable" {
			delta := row.Current.Value - row.Baseline.Value
			row.Delta = &delta
		}
		out.Rows = append(out.Rows, row)
	}
	if out.Reason == "comparable" && len(out.Rows) == 0 {
		return out, fmt.Errorf("%w：没有可核对指标", ErrState)
	}
	proof := func(d assistantBenchmarkSource) []any {
		return []any{d.Profile, d.ProfileVersion, d.PlanRevision, d.Step, d.Operation, d.Job, d.Connection, d.Capability,
			d.CapabilityVersion, d.Pin, d.Phase, d.State, d.JobBound, d.Status, d.Spec, d.Targets, digest(d.Raw)}
	}
	out.Proof = digest(toJSON([]any{req, proof(baseline), proof(current), out.Reason}))
	return out, nil
}
