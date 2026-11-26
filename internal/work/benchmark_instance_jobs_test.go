package work

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type benchmarkJobRoundTrip func(*http.Request) (*http.Response, error)

func (f benchmarkJobRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestBenchmarkInstanceJobBindingSurvivesRestartAndRejectsCrossAccountClaim(t *testing.T) {
	ctx := t.Context()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	instancePath := filepath.Join(dir, "instance.sqlite")
	instance, err := Open(instancePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instance.Close() })
	if err := instance.EnsureAccountMode(ctx, true); err != nil {
		t.Fatal(err)
	}
	alice, err := instance.BootstrapAccount(ctx, "alice", "Alice", "fixture-hash")
	if err != nil {
		t.Fatal(err)
	}
	if err := instance.CreateAccountSession(ctx, alice.PrincipalID, "fixture-hash", "fixture-session", now()+60000); err != nil {
		t.Fatal(err)
	}
	bob, err := instance.CreateAccount(ctx, "fixture-session", "bob", "Bob", "fixture-member-hash", false)
	if err != nil {
		t.Fatal(err)
	}
	const claimedJob = "provider-job-001"
	type fixture struct {
		store                             *Store
		provider                          *HTTPBenchmarkProvider
		path, principal, task, requestKey string
		counts                            map[string]int
		completed                         bool
	}
	create := func(account Account, base string, revision int, serial string) *fixture {
		t.Helper()
		f := &fixture{principal: account.PrincipalID, path: filepath.Join(dir, account.Username+".sqlite"), counts: make(map[string]int)}
		f.provider, err = NewHTTPBenchmarkProvider(base, "fixture-"+account.Username, revision, []string{"test"}, "fixture-token-"+account.Username)
		if err != nil {
			t.Fatal(err)
		}
		// Exercise the real HTTP adapter's body, endpoint, parser and pinned
		// provider without sockets, external services or node credentials.
		f.provider.client.Transport = benchmarkJobRoundTrip(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost || req.Header.Get("Authorization") != "Bearer fixture-token-"+account.Username {
				t.Fatalf("unexpected HTTP call: %s %s", req.Method, req.URL.Path)
			}
			var payload struct {
				BenchmarkJobRequest
				JobID string `json:"job_id"`
			}
			if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			action := strings.TrimPrefix(req.URL.Path, "/api/")
			f.counts[action]++
			if action == "start" {
				if f.requestKey != "" || payload.IdempotencyKey == "" {
					t.Fatal("worker started twice or lost idempotency key")
				}
				f.requestKey = payload.IdempotencyKey
			} else if payload.IdempotencyKey != f.requestKey {
				t.Fatal("read used a different idempotency key")
			}
			body := `{"job_id":"` + claimedJob + `"}`
			switch action {
			case "start", "lookup":
			case "get":
				if payload.JobID != claimedJob {
					t.Fatal("get used a different provider job")
				}
				state := "running"
				if f.completed {
					state = "completed"
				}
				body = `{"job_id":"` + claimedJob + `","state":"` + state + `","measurements":[]}`
			default:
				t.Fatalf("unexpected action: %s", action)
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		f.store, err = OpenAccountBusinessStore(f.path, instance)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.store.Close() })
		if err := f.store.BindAccountBusinessStore(ctx, f.principal); err != nil {
			t.Fatal(err)
		}
		if err := f.store.InstallBenchmarkProvider(ctx, f.provider); err != nil {
			t.Fatal(err)
		}
		target := account.Username + "-node"
		const validator = benchmarkValidatorV1
		binding := BenchmarkBinding{Connection: f.provider.connection, Revision: f.provider.revision, Tool: "test", Validator: validator}
		grant := DomainBenchmarkGrant{Revision: binding.Revision, Connection: binding.Connection, Tool: binding.Tool, ExpiresAt: now() + 60000}
		entries := []DomainAssetEntry{{ObjectRef: target, Kind: "node", Identity: serial, PhysicalSerial: serial, Benchmark: []DomainBenchmarkGrant{grant}}}
		if err := f.store.ImportDomainAssetsForAccount(ctx, f.principal, entries); err != nil {
			t.Fatal(err)
		}
		profile := BenchmarkProfile{Key: "profile", Version: 1, Cases: []BenchmarkCase{{
			Key: "test", Binding: binding, Targets: []string{target}, RequiredMetrics: map[string]string{"rate": "GB/s"},
		}}}
		if err := f.store.PublishBenchmarkProfile(ctx, f.principal, profile); err != nil {
			t.Fatal(err)
		}
		f.task, err = f.store.SubmitBenchmarkRun(ctx, f.principal, BenchmarkRunRequest{RequestKey: "run", Profile: "profile", Version: 1}, f.provider)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	a := create(alice, "https://fixture.invalid/api", 1, "SERIAL-A")
	b := create(bob, "https://FIXTURE.invalid:443/api/", 2, "SERIAL-B")
	source := a.provider.benchmarkJobSource()
	if source == "" || source != b.provider.benchmarkJobSource() {
		t.Fatal("ports, connection names or revisions partitioned the same backend")
	}
	secret := strings.Repeat("a", 32)
	receiver, err := a.provider.EventReceiver(secret)
	if err != nil {
		t.Fatal(err)
	}
	signHint := func(stamp, audience string, body []byte) string {
		mac := hmac.New(sha256.New, []byte(secret))
		if audience != "" {
			_, _ = mac.Write([]byte(audience + "\n"))
		}
		_, _ = mac.Write([]byte(stamp + "\n"))
		_, _ = mac.Write(body)
		return hex.EncodeToString(mac.Sum(nil))
	}
	sendHint := func(body []byte) (bool, error) {
		stamp := strconv.FormatInt(now(), 10)
		return receiver.receiveAccountHint(ctx, instance, stamp, signHint(stamp, accountBenchmarkHintPrefix+source, body), body)
	}
	early := []byte(`{"event_id":"early-hint","job_id":"` + claimedJob + `"}`)
	if duplicate, err := sendHint(early); err != nil || duplicate {
		t.Fatalf("receive before any job mapping: %v %v", duplicate, err)
	}
	if duplicate, err := sendHint(early); err != nil || !duplicate {
		t.Fatalf("receive retransmission: %v %v", duplicate, err)
	}
	if _, err := sendHint([]byte(`{"event_id":"early-hint","job_id":"other-job"}`)); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed content reused event ID: %v", err)
	}
	if _, err := sendHint([]byte(`{"event_id":"spoof","job_id":"provider-job-001","account_id":"bob"}`)); !errors.Is(err, ErrBenchmarkInput) {
		t.Fatalf("callback could supply account: %v", err)
	}
	stamp := strconv.FormatInt(now(), 10)
	if _, err := receiver.receiveAccountHint(ctx, instance, stamp, signHint(stamp, "", early), early); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("source-unbound legacy signature passed: %v", err)
	}
	if _, err := receiver.receiveAccountHint(ctx, instance, stamp, signHint(stamp, accountBenchmarkHintPrefix+"other", early), early); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("signature for another source passed: %v", err)
	}
	stale := strconv.FormatInt(now()-301000, 10)
	if _, err := receiver.receiveAccountHint(ctx, instance, stale, signHint(stale, accountBenchmarkHintPrefix+source, early), early); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("expired signature passed: %v", err)
	}
	if _, err := receiver.Receive(ctx, instance, stamp, signHint(stamp, "", early), early); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("direct receiver accepted multi-account instance: %v", err)
	}

	// Simulate a crash exactly after the central job identity committed:
	// private claim and HTTP Start succeeded, but private receipt never commits.
	d, selected, err := a.store.claimBenchmark(ctx, a.provider, now())
	if err != nil || !selected || d.action != "start" {
		t.Fatalf("claim A: %+v %v %v", d, selected, err)
	}
	job, err := a.provider.Start(ctx, d.request)
	if err != nil || job != claimedJob {
		t.Fatalf("start: %q %v", job, err)
	}
	op, err := readOperation(a.store.db, d.operation)
	if err != nil {
		t.Fatal(err)
	}
	binding := d.instanceJob(job, op)
	if accepted, err := bindInstanceBenchmarkJob(ctx, instance.db, binding); err != nil || !accepted {
		t.Fatalf("first binding: %v %v", accepted, err)
	}
	if err := a.store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := instance.Close(); err != nil {
		t.Fatal(err)
	}
	instance, err = Open(instancePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []*fixture{a, b} {
		f.store, err = OpenAccountBusinessStore(f.path, instance)
		if err != nil {
			t.Fatal(err)
		}
	}
	snapshot := func(f *fixture) BenchmarkCaseProgress {
		t.Helper()
		p, err := f.store.BenchmarkProgress(ctx, f.principal, f.task)
		if err != nil || len(p.Cases) != 1 {
			t.Fatalf("snapshot: %+v %v", p, err)
		}
		return p.Cases[0]
	}
	tick := func(f *fixture) {
		t.Helper()
		if _, err := f.store.db.Exec("UPDATE benchmark_cases SET next_at=0 WHERE task_id=?", f.task); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.runBenchmarkOnce(ctx, f.provider); err != nil {
			t.Fatal(err)
		}
	}
	tick(a)
	if x := snapshot(a); x.Phase != "following" || x.JobID != claimedJob || a.counts["start"] != 1 || a.counts["lookup"] != 1 {
		t.Fatalf("restart failed to adopt exact durable replay: %+v %+v", x, a.counts)
	}
	assertLocalReceipt := func(f *fixture, event string, want int) {
		t.Helper()
		var count int
		if err := f.store.db.QueryRow("SELECT count(*) FROM inbox_events WHERE kind=? AND event_id=? AND state='applied'", accountBenchmarkHintKind, event).Scan(&count); err != nil || count != want {
			t.Fatalf("local hint receipt principal=%s want=%d got=%d err=%v", f.principal, want, count, err)
		}
	}
	setDue := func(f *fixture, at, inFlight int64) {
		t.Helper()
		if _, err := f.store.db.Exec("UPDATE benchmark_cases SET next_at=?,call_until=? WHERE task_id=?", at, inFlight, f.task); err != nil {
			t.Fatal(err)
		}
	}
	makeHintDue := func(event string) {
		t.Helper()
		if _, err := instance.db.Exec("UPDATE inbox_events SET next_retry_at=0 WHERE event_id=?", event); err != nil {
			t.Fatal(err)
		}
	}
	later := now() + 60000
	if err := b.store.applyAccountBenchmarkHints(ctx, now()); err != nil {
		t.Fatal(err)
	}
	assertLocalReceipt(b, "early-hint", 0)
	assertLocalReceipt(a, "early-hint", 0)
	makeHintDue("early-hint")
	setDue(a, later, later)
	if err := a.store.applyAccountBenchmarkHints(ctx, now()); err != nil {
		t.Fatal(err)
	}
	assertLocalReceipt(a, "early-hint", 0)
	makeHintDue("early-hint")
	setDue(a, later, 0)
	if _, err := a.store.runBenchmarkOnce(ctx, a.provider); err != nil {
		t.Fatal(err)
	}
	assertLocalReceipt(a, "early-hint", 1)
	if x := snapshot(a); x.Phase != "following" || a.counts["get"] != 1 {
		t.Fatalf("authenticated hint must only wake one original GET: %+v %+v", x, a.counts)
	}
	// Simulate process loss after the private one-time wake, but before the
	// instance receipt acknowledgement. Restart must only acknowledge it.
	if duplicate, err := sendHint([]byte(`{"event_id":"ack-loss","job_id":"` + claimedJob + `"}`)); err != nil || duplicate {
		t.Fatalf("receive ack-loss fixture: %v %v", duplicate, err)
	}
	setDue(a, later, 0)
	pending, err := pendingAccountBenchmarkHints(ctx, instance.db, a.principal, now())
	if err != nil || len(pending) != 1 {
		t.Fatalf("expected one pending hint: %+v %v", pending, err)
	}
	private, err := a.store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := wakeAccountBenchmark(ctx, private, pending[0], now())
	if err != nil || !applied {
		private.Rollback()
		t.Fatalf("first wake did not commit: %v %v", applied, err)
	}
	if err := private.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := a.store.Close(); err != nil {
		t.Fatal(err)
	}
	a.store, err = OpenAccountBusinessStore(a.path, instance)
	if err != nil {
		t.Fatal(err)
	}
	setDue(a, later, 0)
	if _, err := a.store.runBenchmarkOnce(ctx, a.provider); err != nil || a.counts["get"] != 1 {
		t.Fatalf("redelivery issued another GET: count=%d err=%v", a.counts["get"], err)
	}
	assertLocalReceipt(a, "ack-loss", 1)
	var acknowledged string
	if err := instance.db.QueryRow("SELECT state FROM inbox_events WHERE event_id='ack-loss'").Scan(&acknowledged); err != nil || acknowledged != "applied" {
		t.Fatalf("instance did not acknowledge redelivery: %s %v", acknowledged, err)
	}
	a.completed = true
	tick(a)
	if x := snapshot(a); x.Phase != "incomplete" || a.counts["get"] != 2 {
		t.Fatalf("terminal get: %+v %+v", x, a.counts)
	}
	// Another independently authorized operation can use its own asset,
	// but the provider's truncated/reused ID can't bind or route to it.
	tick(b)
	tick(b)
	if x := snapshot(b); x.Phase != "submitting" || x.JobID != "" || x.Operation == nil || x.Operation.State != string(RuntimeOperationReconcile) || x.TargetHolds != 1 ||
		b.counts["start"] != 1 || b.counts["lookup"] != 1 || b.counts["get"] != 0 {
		t.Fatalf("B accepted A's provider job or reissued Start: %+v %+v", x, b.counts)
	}
	if owner, err := readInstanceBenchmarkJob(ctx, instance.db, source, claimedJob); err != nil || owner != binding {
		t.Fatalf("instance owner overwritten: %+v %v", owner, err)
	}
	other := binding
	other.job = "provider-job-replacement"
	if accepted, err := bindInstanceBenchmarkJob(ctx, instance.db, other); err != nil || accepted {
		t.Fatalf("operation could change external job: %v %v", accepted, err)
	}
	other.operation = "different-operation"
	if accepted, err := bindInstanceBenchmarkJob(ctx, instance.db, other); err != nil || accepted {
		t.Fatalf("idempotency key could change operation/job: %v %v", accepted, err)
	}

	r, err := a.provider.EventReceiver(secret)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"event_id":"signed-offline-hint","job_id":"` + claimedJob + `"}`)
	stamp = strconv.FormatInt(now(), 10)
	signature := hmac.New(sha256.New, []byte(secret))
	_, _ = signature.Write([]byte(stamp + "\n"))
	_, _ = signature.Write(body)
	if _, err := r.Receive(ctx, b.store, stamp, hex.EncodeToString(signature.Sum(nil)), body); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("unrouted receiver admitted even valid hint to wrong account: %v", err)
	}
}
