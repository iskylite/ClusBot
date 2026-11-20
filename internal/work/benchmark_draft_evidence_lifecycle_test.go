package work

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
)

const draftEvidenceDeepEPLog = "DeepEP test fixture: GPUs=40 dispatch rank=0 tokens=4096 BW=100 GB/s\n"

type draftEvidenceFixtureProvider struct{ benchFixtureProvider }

func (p *draftEvidenceFixtureProvider) Get(ctx context.Context, request BenchmarkJobRequest, job string) (BenchmarkJobResult, error) {
	result, err := p.benchFixtureProvider.Get(ctx, request, job)
	if err == nil && !p.missingEvidence {
		result.Measurements[0].Raw = &BenchmarkRawArtifact{
			Ref: "deepep.stdout", Bytes: []byte(draftEvidenceDeepEPLog), SHA256: digest(draftEvidenceDeepEPLog),
		}
	}
	return result, err
}

func TestBenchmarkDraftEvidenceRequirementReachesNewRunsOnly(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "benchmark-draft-evidence.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const actor, key = "user:session", "pod7-fixture"
	binding := BenchmarkBinding{Connection: "bench-test", Tool: "test", Revision: 1, Validator: benchmarkValidatorV1}
	grant := DomainBenchmarkGrant{Revision: 1, Connection: binding.Connection, Tool: binding.Tool,
		Allowed: map[string][]string{"gpu_count": {"40"}}, ExpiresAt: now() + 120000}
	if err := s.ImportDomainAssets(ctx, []DomainAssetEntry{{ObjectRef: "node-1", Kind: "node", Identity: "node-fixture", Benchmark: []DomainBenchmarkGrant{grant}}}); err != nil {
		t.Fatal(err)
	}
	original := BenchmarkCase{Key: "deepep", Binding: binding, Targets: []string{"node-1"},
		Parameters: map[string]string{"gpu_count": "40"}, ActualConfig: map[string]string{"gpu_count": "40"},
		RequiredMetrics: map[string]string{"rate": "GB/s"}, Threshold: &BenchmarkMetricThreshold{Metric: "rate", Minimum: 80}}
	if err := s.PublishBenchmarkProfile(ctx, actor, BenchmarkProfile{Key: key, Version: 1, Cases: []BenchmarkCase{original}}); err != nil {
		t.Fatal(err)
	}
	admit := func(version int, request string, p BenchmarkProvider) string {
		t.Helper()
		task, err := s.SubmitBenchmarkRun(ctx, actor, BenchmarkRunRequest{RequestKey: request, Profile: key, Version: version}, p)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	run := func(p BenchmarkProvider) {
		t.Helper()
		if acted, err := s.runBenchmarkOnce(ctx, p); err != nil || !acted {
			t.Fatalf("advancement: %t %v", acted, err)
		}
	}
	progress := func(task string) BenchmarkRunProgress {
		t.Helper()
		out, err := s.BenchmarkProgress(ctx, actor, task)
		if err != nil || len(out.Cases) != 1 {
			t.Fatalf("progress: %+v %v", out, err)
		}
		return out
	}
	preflight := &draftEvidenceFixtureProvider{benchFixtureProvider{jobOverride: "older-job", missingEvidence: true}}
	older := admit(1, "older-production-shaped-fixture", preflight)
	run(preflight)
	before := progress(older)
	old := before.Cases[0]
	if old.JobID == "" || old.Operation == nil || old.Wait == nil || old.Wait.State != "open" || old.TargetHolds != 1 {
		t.Fatalf("old task is not in flight before authoring v2: %+v", old)
	}

	draft, err := s.StartBenchmarkDraftFromPublished(ctx, actor, key, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	candidate := original
	candidate.NeedsRawEvidence = true
	candidate.Threshold = &BenchmarkMetricThreshold{Metric: "rate", Minimum: 120}
	preview, err := s.PreviewBenchmarkDraftStructuredEdit(ctx, actor, key, BenchmarkDraftRequest{
		ExpectedRevision: draft.Revision, BaseVersion: 1, Cases: []BenchmarkCase{candidate},
	})
	if err != nil || preview.CurrentProfile.Cases[0].NeedsRawEvidence || !preview.ProposedProfile.Cases[0].NeedsRawEvidence ||
		!slices.Equal(preview.Changes.Changed, []string{"deepep"}) {
		t.Fatalf("requirements preview: %+v %v", preview, err)
	}
	if current, err := s.ReadBenchmarkDraft(ctx, actor, key); err != nil || current.Revision != draft.Revision || current.Profile.Cases[0].NeedsRawEvidence {
		t.Fatalf("preview mutated the old draft: %+v %v", current, err)
	}
	saved, err := s.SaveBenchmarkDraft(ctx, actor, key, BenchmarkDraftRequest{BaseVersion: preview.BaseVersion,
		ExpectedRevision: preview.ExpectedRevision, Cases: preview.ProposedProfile.Cases})
	if err != nil || saved.Published || saved.ValidationError != "" {
		t.Fatalf("save v2 draft: %+v %v", saved, err)
	}
	published, err := s.PublishBenchmarkDraft(ctx, actor, key, saved.Revision)
	if err != nil || !published.Published || published.Profile.Version != 2 {
		t.Fatalf("publish v2: %+v %v", published, err)
	}

	withoutRaw := &draftEvidenceFixtureProvider{benchFixtureProvider{jobOverride: "missing-raw-job", missingEvidence: true}}
	missing := admit(2, "new-version-missing-raw", withoutRaw)
	if cur := progress(older).Cases[0]; cur.Operation == nil || cur.Wait == nil || cur.Operation.ID != old.Operation.ID ||
		cur.Wait.ID != old.Wait.ID || cur.TargetHolds != 1 || cur.JobID != old.JobID {
		t.Fatalf("draft publication/admission changed the prior in-flight work: %+v", cur)
	}
	if _, err := s.db.Exec("UPDATE benchmark_cases SET next_at=? WHERE task_id=?", now()+60000, older); err != nil {
		t.Fatal(err)
	}
	if _, err := s.runBenchmarkOnce(ctx, withoutRaw); err != nil {
		t.Fatal(err)
	}
	if cur := progress(missing).Cases[0]; cur.Operation != nil || cur.TargetHolds != 0 || withoutRaw.started != 0 {
		t.Fatalf("new run was optimistically dispatched across existing hold: %+v", cur)
	}
	due := func(task string) {
		t.Helper()
		if _, err := s.db.Exec("UPDATE benchmark_cases SET next_at=1 WHERE task_id=?", task); err != nil {
			t.Fatal(err)
		}
	}
	readReport := func(task string, version int, state string) BenchmarkReportCase {
		t.Helper()
		report, err := s.ReadBenchmarkReport(ctx, actor, task)
		if err != nil || report.Profile.Version != version || report.TaskState != state ||
			report.AssetAcceptance != "not_evaluated" || len(report.Cases) != 1 {
			t.Fatalf("terminal report: %+v %v", report, err)
		}
		out := report.Cases[0]
		if out.ToolOutcome != "completed" || len(out.Targets) != 1 {
			t.Fatalf("tool outcome is independent of evidence acceptance: %+v", out)
		}
		return out
	}
	due(older)
	run(preflight)
	if out := readReport(older, 1, "succeeded"); out.RequiredEvidence != "covered" || out.Performance != "met" ||
		out.Targets[0].VerifiedMetrics["rate"].Value != 100 || preflight.started != 1 {
		t.Fatalf("old case was not assessed by frozen v1 rules: %+v provider=%+v", out, preflight)
	}

	due(missing)
	run(withoutRaw)
	due(missing)
	run(withoutRaw)
	if out := readReport(missing, 2, "failed"); out.RequiredEvidence != "incomplete" ||
		out.Performance != "incomplete" || len(out.Targets[0].VerifiedMetrics) != 0 || withoutRaw.started != 1 {
		t.Fatalf("v2 metrics without required raw were promoted: %+v provider=%+v", out, withoutRaw)
	}
	inventory, err := s.ListBenchmarkEvidence(ctx, actor, missing, "", 1)
	if err != nil || len(inventory.Items) != 0 {
		t.Fatalf("missing raw produced an evidence reference: %+v %v", inventory, err)
	}

	withRaw := &draftEvidenceFixtureProvider{benchFixtureProvider{jobOverride: "complete-raw-job"}}
	withLog := admit(2, "new-version-with-raw", withRaw)
	run(withRaw)
	due(withLog)
	run(withRaw)
	if out := readReport(withLog, 2, "succeeded"); out.RequiredEvidence != "covered" ||
		out.Performance != "not_met" || out.Targets[0].VerifiedMetrics["rate"].Value != 100 || withRaw.started != 1 {
		t.Fatalf("v2 raw restored evidence, but did not preserve v2 threshold: %+v provider=%+v", out, withRaw)
	}
	inventory, err = s.ListBenchmarkEvidence(ctx, actor, withLog, "", 1)
	if err != nil || len(inventory.Items) != 1 || !inventory.Items[0].AssessmentLinked ||
		inventory.Items[0].ProviderJobID != "complete-raw-job" || inventory.Items[0].SHA256 != digest(draftEvidenceDeepEPLog) {
		t.Fatalf("wrong positive evidence inventory: %+v %v", inventory, err)
	}
	raw, err := s.ReadBenchmarkEvidence(ctx, actor, withLog, inventory.Items[0].ID)
	if err != nil || string(raw) != draftEvidenceDeepEPLog {
		t.Fatalf("original evidence bytes did not round-trip: %q %v", raw, err)
	}
}
