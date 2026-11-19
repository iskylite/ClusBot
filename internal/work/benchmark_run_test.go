package work

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

type benchFixtureProvider struct {
	started, looked, read      int
	key, job                   string
	loseReply, missingEvidence bool
	jobOverride                string
	valueOverride              float64
}

func (b *benchFixtureProvider) Supports(binding BenchmarkBinding) bool {
	return binding.Connection == "bench-test" && binding.Revision == 1 && binding.Tool == "test" && binding.Validator == benchmarkValidatorV1
}
func (b *benchFixtureProvider) Start(_ context.Context, r BenchmarkJobRequest) (string, error) {
	b.started++
	b.key = r.IdempotencyKey
	b.job = "job-ID"
	if b.jobOverride != "" {
		b.job = b.jobOverride
	}
	if b.loseReply {
		return "", errors.New("response lost after acceptance")
	}
	return b.job, nil
}
func (b *benchFixtureProvider) Lookup(_ context.Context, r BenchmarkJobRequest) (string, error) {
	b.looked++
	if r.IdempotencyKey != b.key {
		return "", ErrConflict
	}
	return b.job, nil
}
func (b *benchFixtureProvider) Get(_ context.Context, r BenchmarkJobRequest, job string) (BenchmarkJobResult, error) {
	b.read++
	if r.IdempotencyKey != b.key || job != b.job {
		return BenchmarkJobResult{}, ErrConflict
	}
	value := 100.0
	if b.valueOverride != 0 {
		value = b.valueOverride
	}
	m := BenchmarkMeasurement{Target: "node-1", Outcome: "measured", ActualConfig: map[string]string{"gpu_count": "40"},
		Metrics: map[string]BenchmarkMeasuredMetric{"rate": {Value: value, Unit: "GB/s"}}}
	if !b.missingEvidence {
		m.Raw = &BenchmarkRawArtifact{Ref: "stdout", Bytes: []byte("test output"), SHA256: digest("test output")}
	}
	return BenchmarkJobResult{JobID: job, State: "completed", Measurements: []BenchmarkMeasurement{m}}, nil
}

func TestV31BenchmarkLifecycle(t *testing.T) {
	ctx := context.Background()
	for _, missing := range []bool{false, true} {
		name := "verified"
		if missing {
			name = "incomplete"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test.db")
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close() })
			grant := DomainBenchmarkGrant{Revision: 1, Connection: "bench-test", Tool: "test", Allowed: map[string][]string{"gpu_count": {"40"}}, ExpiresAt: now() + 120000}
			if err = s.ImportDomainAssets(ctx, []DomainAssetEntry{{ObjectRef: "node-1", Kind: "node", Identity: "inventory-1", Benchmark: []DomainBenchmarkGrant{grant}}}); err != nil {
				t.Fatal(err)
			}
			actor := "user:session"
			binding := BenchmarkBinding{Connection: "bench-test", Tool: "test", Revision: 1, Validator: benchmarkValidatorV1}
			c := BenchmarkCase{Key: "single-node", Binding: binding, Targets: []string{"node-1"}, Parameters: map[string]string{"gpu_count": "40"},
				ActualConfig: map[string]string{"gpu_count": "40"}, RequiredMetrics: map[string]string{"rate": "GB/s"}, NeedsRawEvidence: true,
				Threshold: &BenchmarkMetricThreshold{Metric: "rate", Minimum: 120}}
			p := BenchmarkProfile{Key: "profile", Version: 1, Cases: []BenchmarkCase{c}}
			if err = s.PublishBenchmarkProfile(ctx, actor, p); err != nil {
				t.Fatal(err)
			}
			p.Cases[0].Parameters = map[string]string{"gpu_count": "72"}
			p.Cases[0].ActualConfig = map[string]string{"gpu_count": "72"}
			if err = s.PublishBenchmarkProfile(ctx, actor, p); !errors.Is(err, ErrConflict) {
				t.Fatalf("published profile overwritten: %v", err)
			}
			p.Version = 2
			if err = s.PublishBenchmarkProfile(ctx, actor, p); err != nil {
				t.Fatal(err)
			}
			provider := &benchFixtureProvider{loseReply: true, missingEvidence: missing}
			if _, err = s.SubmitBenchmarkRun(ctx, actor, BenchmarkRunRequest{"deny", "profile", 2}, provider); !errors.Is(err, ErrAuthorizationDenied) {
				t.Fatalf("unauthorized params: %v", err)
			}
			request := BenchmarkRunRequest{"request", "profile", 1}
			task, err := s.SubmitBenchmarkRun(ctx, actor, request, provider)
			if err != nil {
				t.Fatal(err)
			}
			if again, err := s.SubmitBenchmarkRun(ctx, actor, request, provider); err != nil || again != task {
				t.Fatalf("duplicate created %q: %v", again, err)
			}
			if _, err := s.BenchmarkProgress(ctx, "another-actor", task); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("foreign task read: %v", err)
			}
			queued, err := s.BenchmarkProgress(ctx, actor, task)
			if err != nil || len(queued.Cases) != 1 {
				t.Fatalf("queued snapshot: %+v %v", queued, err)
			}
			if c := queued.Cases[0]; len(c.Targets) != 1 || c.Targets[0].Ref != "node-1" || c.Targets[0].FrozenRevision < 1 || c.Targets[0].LocalHold != "not_locally_held" || c.Phase != "queued" || c.StepID == "" || c.Operation != nil || c.Wait != nil || c.TargetHolds != 0 {
				t.Fatalf("queued case: %+v", c)
			}
			run := func() {
				t.Helper()
				if ok, e := s.runBenchmarkOnce(ctx, provider); !ok || e != nil {
					t.Fatalf("run step %v %v", ok, e)
				}
			}
			run()
			inFlight, err := s.BenchmarkProgress(ctx, actor, task)
			if err != nil || len(inFlight.Cases) != 1 {
				t.Fatalf("in-flight snapshot: %+v %v", inFlight, err)
			}
			if c := inFlight.Cases[0]; c.Phase != "submitting" || c.Operation == nil || c.Operation.State != "reconcile" ||
				c.Wait == nil || c.Wait.State != "open" || c.Wait.Version != 1 || c.Wait.RecheckAt <= 0 || c.TargetHolds != 1 || len(c.Targets) != 1 || c.Targets[0].LocalHold != "held_by_case" {
				t.Fatalf("unknown effect must keep open wait and hold: %+v operation=%+v wait=%+v", c, c.Operation, c.Wait)
			}
			if provider.started != 1 {
				t.Fatalf("got %d starts", provider.started)
			}
			pendingReport, err := s.ReadBenchmarkReport(ctx, actor, task)
			if err != nil || pendingReport.Cases[0].ToolOutcome != "outcome_unknown" || pendingReport.Cases[0].RequiredEvidence != "not_evaluated" {
				t.Fatalf("in-flight report %+v: %v", pendingReport, err)
			}

			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.RecoverV3(ctx, now()); err != nil {
				t.Fatal(err)
			}
			if _, err = s.db.Exec("UPDATE benchmark_cases SET next_at=0 WHERE task_id=?", task); err != nil {
				t.Fatal(err)
			}
			run()
			run()
			got, err := s.BenchmarkProgress(ctx, actor, task)
			if err != nil || len(got.Cases) != 1 || got.Cases[0].Phase != name || got.Cases[0].JobID != "job-ID" {
				t.Fatalf("progress %+v %v: want %s", got, err, name)
			}
			if c := got.Cases[0]; c.Operation == nil || c.Wait == nil || c.Wait.State != "closed" || c.TargetHolds != 0 || len(c.Targets) != 1 || c.Targets[0].LocalHold != "not_locally_held" {
				t.Fatalf("terminal case: %+v operation=%+v wait=%+v", c, c.Operation, c.Wait)
			}
			if provider.started != 1 || provider.looked != 1 || provider.read != 1 {
				t.Fatalf("unexpected remote calls %+v", provider)
			}
			if got.State != "failed" && missing || got.State != "succeeded" && !missing {
				t.Fatalf("wrong terminal task state %+v", got)
			}
			report, err := s.ReadBenchmarkReport(ctx, actor, task)
			if err != nil || report.SchemaVersion != 1 || report.AssetAcceptance != "not_evaluated" || report.PlanRevision != got.PlanRevision || len(report.Cases) != 1 {
				t.Fatalf("report %+v: %v", report, err)
			}
			item := report.Cases[0]
			if item.ToolOutcome != "completed" || len(item.Targets) != 1 {
				t.Fatalf("remote success is not measurement success: %+v", item)
			}
			if missing {
				if item.RequiredEvidence != "incomplete" || item.Performance != "incomplete" || len(item.Targets[0].VerifiedMetrics) != 0 {
					t.Fatalf("missing evidence promoted: %+v", item)
				}
			} else if item.RequiredEvidence != "covered" || item.Performance != "not_met" ||
				item.Targets[0].VerifiedMetrics["rate"] != (BenchmarkMeasuredMetric{Value: 100, Unit: "GB/s"}) {
				t.Fatalf("verified measurements and threshold not distinct: %+v", item)
			}
			if _, err := s.ReadBenchmarkReport(ctx, "other", task); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("foreign actor read metrics: %v", err)
			}

			page, err := s.ListBenchmarkEvidence(ctx, actor, task, "", 1)
			if err != nil || page.TaskID != task || (missing && len(page.Items) != 0) || (!missing && len(page.Items) != 1) {
				t.Fatalf("inventory %+v: %v", page, err)
			}
			if _, err := s.ListBenchmarkEvidence(ctx, "other", task, "", 1); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("foreign evidence inventory exists: %v", err)
			}
			if !missing {
				item := page.Items[0]
				if !item.AssessmentLinked || item.CaseKey != "single-node" || item.StepID != got.Cases[0].StepID ||
					item.OperationID != got.Cases[0].Operation.ID || item.ProviderJobID != "job-ID" || item.Connection != "bench-test" ||
					item.SHA256 != digest("test output") || item.SizeBytes != int64(len("test output")) {
					t.Fatalf("untrusted inventory metadata: %+v", item)
				}
				if bytes, err := s.ReadBenchmarkEvidence(ctx, actor, task, item.ID); err != nil || string(bytes) != "test output" {
					t.Fatalf("original bytes changed: %q %v", bytes, err)
				}
				if _, err := s.ReadBenchmarkEvidence(ctx, "other", task, item.ID); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("foreign evidence exposed: %v", err)
				}
				extra := digest("extra test artifact")
				if _, err := s.db.Exec("INSERT INTO benchmark_evidence VALUES(?,?,?,?,?,?)", extra, item.StepID, item.Target, "outside-assessment", digest("extra"), []byte("extra")); err != nil {
					t.Fatal(err)
				}
				page, err = s.ListBenchmarkEvidence(ctx, actor, task, "", 1)
				if err != nil || len(page.Items) != 1 || page.NextAfter != page.Items[0].ID {
					t.Fatalf("missing first page boundary: %+v %v", page, err)
				}
				second, err := s.ListBenchmarkEvidence(ctx, actor, task, page.NextAfter, 1)
				if err != nil || len(second.Items) != 1 || second.Items[0].ID == page.Items[0].ID || second.NextAfter != "" {
					t.Fatalf("wrong second page: %+v %v", second, err)
				}
				if extraItem := append(page.Items, second.Items...); (extraItem[0].ID == extra && extraItem[0].AssessmentLinked) || (extraItem[1].ID == extra && extraItem[1].AssessmentLinked) {
					t.Fatalf("unassessed captured bytes promoted: %+v", extraItem)
				}
				if _, err := s.db.Exec("DELETE FROM benchmark_evidence WHERE artifact_id IN (?,?)", item.ID, extra); err != nil {
					t.Fatal(err)
				}
				empty, err := s.ListBenchmarkEvidence(ctx, actor, task, "", 1)
				if err != nil || len(empty.Items) != 0 {
					t.Fatalf("no longer present artifacts still visible: %+v %v", empty, err)
				}
			}

			var holds int
			if err = s.db.QueryRow("SELECT count(*) FROM benchmark_target_holds").Scan(&holds); err != nil || holds != 0 {
				t.Fatalf("holds %d %v", holds, err)
			}
		})
	}
}

type stoppingBenchProvider struct {
	benchFixtureProvider
	cancellations int
	terminal      bool
}

func (b *stoppingBenchProvider) Cancel(_ context.Context, r BenchmarkJobRequest, job string) error {
	if r.IdempotencyKey != b.key || job != b.job {
		return ErrConflict
	}
	b.cancellations++
	return nil
}
func (b *stoppingBenchProvider) Get(_ context.Context, _ BenchmarkJobRequest, job string) (BenchmarkJobResult, error) {
	state := "running"
	if b.terminal {
		state = "cancelled"
	}
	return BenchmarkJobResult{JobID: job, State: state}, nil
}

func TestV31BenchmarkCancellationRetainsHold(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "cancel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	actor := "user:session"
	grant := DomainBenchmarkGrant{Revision: 1, Connection: "bench-test", Tool: "test", ExpiresAt: now() + 120000}
	if err = s.ImportDomainAssets(ctx, []DomainAssetEntry{{ObjectRef: "node-1", Kind: "node", Identity: "inventory-1", Benchmark: []DomainBenchmarkGrant{grant}}}); err != nil {
		t.Fatal(err)
	}
	p := BenchmarkProfile{Key: "cancel", Version: 1, Cases: []BenchmarkCase{{Key: "one", Binding: BenchmarkBinding{"bench-test", 1, "test", benchmarkValidatorV1},
		Targets: []string{"node-1"}, RequiredMetrics: map[string]string{"rate": "GB/s"}}}}
	if err = s.PublishBenchmarkProfile(ctx, actor, p); err != nil {
		t.Fatal(err)
	}
	provider := &stoppingBenchProvider{}
	task, err := s.SubmitBenchmarkRun(ctx, actor, BenchmarkRunRequest{"r", "cancel", 1}, provider)
	if err != nil {
		t.Fatal(err)
	}
	run := func() {
		t.Helper()
		if ok, e := s.runBenchmarkOnce(ctx, provider); !ok || e != nil {
			t.Fatalf("run step %v %v", ok, e)
		}
	}
	run()
	if err = s.Control(ctx, task, "stop", 1); err != nil {
		t.Fatal(err)
	}
	run()
	run()
	checkRecoveryBlocked := func() {
		t.Helper()
		current, err := scanTask(s.db.QueryRow("SELECT "+taskColumns+" FROM tasks WHERE id=?", task))
		if err != nil {
			t.Fatal(err)
		}
		for i, attempt := range []func(context.Context, string, string, int) (Run, error){s.Resume, s.RetryTask, s.ReassignTask} {
			if created, err := attempt(ctx, task, fmt.Sprintf("generic-recovery-%d", i), current.Revision); !errors.Is(err, ErrBenchmarkRecoveryUnsupported) || created.ID != "" {
				t.Fatalf("generic recovery %d created a model run for the benchmark: %+v %v", i, created, err)
			}
		}
		// Router's resume intent has the same transactional entry; a typed
		// fake decision cannot create a model run for this domain task.
		var n int
		if err := s.db.QueryRow("SELECT count(*) FROM runs WHERE task_id=?", task).Scan(&n); err != nil || n != 0 {
			t.Fatalf("unexpected benchmark model run: %d %v", n, err)
		}
	}
	checkRecoveryBlocked()
	var holds int
	if err = s.db.QueryRow("SELECT count(*) FROM benchmark_target_holds").Scan(&holds); err != nil || holds != 1 {
		t.Fatalf("released hold before stop confirmed: %d %v", holds, err)
	}
	pg, err := s.BenchmarkProgress(ctx, actor, task)
	if err != nil || pg.State != "finalizing" || !pg.StopRequested || len(pg.Cases) != 1 || pg.Cases[0].TargetHolds != 1 || pg.Cases[0].Wait == nil || pg.Cases[0].Wait.State != "open" {
		t.Fatalf("stop still pending: %+v, %v", pg, err)
	}
	if _, err = s.db.Exec("UPDATE benchmark_cases SET next_at=0 WHERE task_id=?", task); err != nil {
		t.Fatal(err)
	}
	provider.terminal = true
	run()
	pg, err = s.BenchmarkProgress(ctx, actor, task)
	if err != nil || pg.State != "cancelled" || provider.cancellations != 1 || !pg.StopRequested || len(pg.Cases) != 1 || pg.Cases[0].TargetHolds != 0 || pg.Cases[0].Wait == nil || pg.Cases[0].Wait.State != "closed" {
		t.Fatalf("wrong stop outcome: %+v / %d, %v", pg, provider.cancellations, err)
	}
	checkRecoveryBlocked()
	current, err := scanTask(s.db.QueryRow("SELECT "+taskColumns+" FROM tasks WHERE id=?", task))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	decision := submissionClassification{DelegationResult: DelegationResult{Route: &RouteDecision{Kind: "resume", TaskID: task, Revision: current.Revision}}}
	if _, err := submitWorkTx(ctx, tx, "router-benchmark-recovery", "fixture", "继续原基准", "", "", false, Delivery{}, &decision, nil); !errors.Is(err, ErrBenchmarkRecoveryUnsupported) {
		t.Fatalf("router resume bypassed the benchmark guard: %v", err)
	}
}

func TestBenchmarkGenericRouteKeepsDedicatedTask(t *testing.T) {
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "benchmark-continuation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const actor = "user:session"
	binding := BenchmarkBinding{Connection: "bench-test", Revision: 1, Tool: "test", Validator: benchmarkValidatorV1}
	grant := DomainBenchmarkGrant{Revision: 1, Connection: binding.Connection, Tool: binding.Tool, ExpiresAt: now() + 120000}
	if err := s.ImportDomainAssets(ctx, []DomainAssetEntry{{ObjectRef: "node-1", Kind: "node", Identity: "local-fixture", Benchmark: []DomainBenchmarkGrant{grant}}}); err != nil {
		t.Fatal(err)
	}
	cases := []BenchmarkCase{{Key: "first", Binding: binding, Targets: []string{"node-1"}, RequiredMetrics: map[string]string{"rate": "GB/s"}}}
	if err := s.PublishBenchmarkProfile(ctx, actor, BenchmarkProfile{Key: "guard-bench", Version: 1, Cases: cases}); err != nil {
		t.Fatal(err)
	}
	provider := &benchFixtureProvider{loseReply: true}
	for _, route := range []string{"implicit-held", "implicit-chat", "explicit-task", "router-followup", "router-goal", "edit-goal"} {
		t.Run(route, func(t *testing.T) {
			task, err := s.SubmitBenchmarkRun(ctx, actor, BenchmarkRunRequest{RequestKey: "benchmark-" + route, Profile: "guard-bench", Version: 1}, provider)
			if err != nil {
				t.Fatal(err)
			}
			if route == "implicit-held" {
				if worked, err := s.runBenchmarkOnce(ctx, provider); err != nil || !worked {
					t.Fatalf("local fake in-flight setup: %v / %v", worked, err)
				}
			}
			priorProgress, err := s.BenchmarkProgress(ctx, actor, task)
			if err != nil || len(priorProgress.Cases) != 1 || (route == "implicit-held" && priorProgress.Cases[0].TargetHolds != 1) {
				t.Fatalf("invalid benchmark baseline: %+v / %v", priorProgress, err)
			}
			before, err := s.Detail(task, true)
			if err != nil {
				t.Fatal(err)
			}
			goal := "错误地改成其他目标"
			switch route {
			case "implicit-chat", "implicit-held", "explicit-task":
				run, err := s.SubmitWithDelivery(ctx, "submit-"+route, "问一下基准怎么做", "", before.Conversation.ID, route == "explicit-task", Delivery{})
				if route == "explicit-task" {
					if !errors.Is(err, ErrBenchmarkRecoveryUnsupported) || run.ID != "" {
						t.Fatalf("explicit generic action must conflict: %+v / %v", run, err)
					}
				} else {
					if err != nil || run.ID == "" || run.TaskID != "" || run.ConversationID != before.Conversation.ID {
						t.Fatalf("discussion must remain independent: %+v / %v", run, err)
					}
					claimed, err := s.claim(ctx)
					if err != nil || claimed.ID != run.ID || claimed.TaskID != "" {
						t.Fatalf("discussion not independently claimed: %+v / %v", claimed, err)
					}
					s.execute(ctx, testProvider(func(_ context.Context, _ []Message, asTask bool, _ int, emit func(string) error) (*TokenUsage, error) {
						if asTask {
							return nil, ErrState
						}
						return nil, emit("独立的解释")
					}), claimed, RunLimits{OutputTokens: 64, ActiveTime: time.Minute, ModelTimeout: time.Minute})
					if events, res, err := s.Events(run.ID, 0); err != nil || res.Status != "completed" || len(events) == 0 {
						t.Fatalf("unrelated discussion did not complete: %+v / %v", res, err)
					}
				}

			case "router-followup", "router-goal":
				kind := "follow_up"
				if route == "router-goal" {
					kind = "set_goal"
				}
				routed, err := s.SubmitRoutedChat(ctx, "submit-"+route, "调整"+before.Task.Title, "", "", Delivery{}, func(_ context.Context, text string, _ []Message, _ []RouteCandidate, _ int) (DelegationResult, error) {
					decision := RouteDecision{Kind: kind, TaskID: task, Revision: before.Task.Revision, Fragment: text}
					if kind == "set_goal" {
						decision.Goal = goal
					}
					return DelegationResult{Route: &decision}, nil
				})
				if route == "router-followup" && (!errors.Is(err, ErrBenchmarkRecoveryUnsupported) || routed.ID != "") {
					t.Fatalf("generic Router follow-up not refused: %+v / %v", routed, err)
				}

			case "edit-goal":
				_, _ = s.EditTask(ctx, task, TaskEdit{Revision: before.Task.Revision, Goal: &goal})
			}
			after, err := s.Detail(task, true)
			if err != nil {
				t.Fatal(err)
			}
			var generic int
			if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM runs WHERE task_id=?", task).Scan(&generic); err != nil {
				t.Fatal(err)
			}
			progress, err := s.BenchmarkProgress(ctx, actor, task)
			if err != nil || !reflect.DeepEqual(priorProgress, progress) {
				t.Fatalf("dedicated case disturbed: %+v / %v", progress, err)
			}
			if generic != 0 || *after.Task != *before.Task || after.Run != nil {
				t.Errorf("generic=%d; mirror before=%+v after=%+v", generic, before.Task, after.Task)
			}
		})
	}
}
