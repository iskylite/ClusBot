package work

import (
	"errors"
	"testing"
)

func TestBenchmarkFrozenGrantCannotBeDisplacedOrReplaced(t *testing.T) {
	for _, scenario := range []string{"active", "revoked", "expired"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := t.Context()
			s, _, task, cases, provider := newDispatchGateFixture(t, "first", "side")
			before, err := s.BenchmarkProgress(ctx, dispatchGateFixtureActor, task)
			if err != nil {
				t.Fatal(err)
			}
			first, side := before.Cases[0].StepID, before.Cases[1].StepID
			var pinned string
			if err := s.db.QueryRow(`SELECT json_extract(authorization_refs, '$."node-1"') FROM benchmark_cases WHERE step_id=?`, first).Scan(&pinned); err != nil || pinned == "" {
				t.Fatalf("missing pinned A: %q %v", pinned, err)
			}
			activeFrom := now() - 1000
			c := cases[0]
			additive := DomainBenchmarkGrant{Revision: c.Binding.Revision, Connection: c.Binding.Connection, Tool: c.Binding.Tool,
				ValidFrom: &activeFrom, ExpiresAt: now() + 150000}
			if err := s.ImportDomainAssets(ctx, []DomainAssetEntry{{ObjectRef: "node-1", Kind: "node", Identity: "fixture", Benchmark: []DomainBenchmarkGrant{additive}}}); err != nil {
				t.Fatal(err)
			}
			// The two equivalent grants have identical policy version.
			// Force B to sort before every possible generated hex ID so
			// the old random-ID dependent bug is reproduced deterministically.
			const b = "!additive-grant-b"
			if res, err := s.db.Exec("UPDATE authorizations SET auth_id=? WHERE action='benchmark.execute' AND auth_id<>?", b, pinned); err != nil {
				t.Fatal(err)
			} else if n, err := res.RowsAffected(); err != nil || n != 1 {
				t.Fatalf("expected one additive B: %d %v", n, err)
			}
			switch scenario {
			case "revoked":
				if err := s.RevokeAuthorizationByID(ctx, pinned, dispatchGateFixtureActor); err != nil {
					t.Fatal(err)
				}
			case "expired":
				if _, err := s.db.Exec("UPDATE authorizations SET expires_at=? WHERE auth_id=?", now(), pinned); err != nil {
					t.Fatal(err)
				}
			}
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			selected, err := benchmarkAuthorization(tx, dispatchGateFixtureActor, "node-1", c, now())
			tx.Rollback()
			if err != nil || selected != b {
				t.Fatalf("fixture must choose B for new admission: %q %v", selected, err)
			}

			hold := dispatchGateInput(benchmarkHoldBeforeDispatch, "preserve-original-hold", 1, 0)
			got, err := s.ApplyBenchmarkExecutionControl(ctx, dispatchGateFixtureActor, task, first, hold)
			if scenario == "active" {
				if err != nil || got.State != benchmarkDispatchHeld {
					t.Fatalf("additive B displaced still-valid A for hold: %+v %v", got, err)
				}
				resume := dispatchGateInput(benchmarkResumeBeforeDispatch, "preserve-original-resume", 1, 1)
				if got, err := s.ApplyBenchmarkExecutionControl(ctx, dispatchGateFixtureActor, task, first, resume); err != nil || got.State != benchmarkDispatchEligible {
					t.Fatalf("additive B displaced still-valid A for resume: %+v %v", got, err)
				}
				skip := BenchmarkExecutionControlInput{Action: benchmarkSkipThisRun, ExpectedPlanRevision: 1,
					RequestID: "preserve-original-skip", Reason: "本次跳过另一个尚未派发测项"}
				if got, err := s.ApplyBenchmarkExecutionControl(ctx, dispatchGateFixtureActor, task, side, skip); err != nil || got.State != benchmarkSkippedThisRun {
					t.Fatalf("additive B displaced still-valid A for skip: %+v %v", got, err)
				}
			} else if !errors.Is(err, ErrAuthorizationDenied) {
				t.Fatalf("controlled operation fell back to B after A became %s: %+v %v", scenario, got, err)
			}

			dispatched, ok, err := s.claimBenchmark(ctx, provider, now())
			if err != nil || !ok || dispatched.step != first {
				t.Fatalf("did not adjudicate original queued case: %+v %t %v", dispatched, ok, err)
			}
			progress, err := s.BenchmarkProgress(ctx, dispatchGateFixtureActor, task)
			if err != nil {
				t.Fatal(err)
			}
			p := progress.Cases[0]
			if scenario == "active" {
				if dispatched.action != "start" || p.Operation == nil || p.Wait == nil || p.TargetHolds != 1 {
					t.Fatalf("additive B displaced still-valid A at dispatch: %+v %+v", dispatched, p)
				}
			} else if dispatched.action != "" || p.Phase != "skipped" || p.Operation != nil || p.Wait != nil || p.TargetHolds != 0 {
				t.Fatalf("new dispatch fell back to B after A became %s: %+v %+v", scenario, dispatched, p)
			}
		})
	}
}
