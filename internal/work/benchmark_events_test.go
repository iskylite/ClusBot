package work

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
)

func TestBenchmarkEarlyNotificationOnlyWakesOriginalJob(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "hints.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	actor := "user:session"
	ctx := context.Background()
	grant := DomainBenchmarkGrant{Revision: 1, Connection: "bench-test", Tool: "test", ExpiresAt: now() + 120000}
	if err = s.ImportDomainAssets(ctx, []DomainAssetEntry{{ObjectRef: "node-1", Kind: "node", Identity: "inventory-1", Benchmark: []DomainBenchmarkGrant{grant}}}); err != nil {
		t.Fatal(err)
	}
	p := BenchmarkProfile{Key: "early", Version: 1, Cases: []BenchmarkCase{{Key: "one", Binding: BenchmarkBinding{"bench-test", 1, "test", benchmarkValidatorV1},
		Targets: []string{"node-1"}, RequiredMetrics: map[string]string{"rate": "GB/s"}}}}
	if err = s.PublishBenchmarkProfile(ctx, actor, p); err != nil {
		t.Fatal(err)
	}
	provider := &benchFixtureProvider{}
	task, err := s.SubmitBenchmarkRun(ctx, actor, BenchmarkRunRequest{"r", "early", 1}, provider)
	if err != nil {
		t.Fatal(err)
	}
	receiver := BenchmarkEventReceiver{connection: "bench-test", revision: 1, secret: "a test-only standalone secret >=32"}
	sign := func(stamp, body string) string {
		mac := hmac.New(sha256.New, []byte(receiver.secret))
		mac.Write([]byte(stamp + "\n" + body))
		return hex.EncodeToString(mac.Sum(nil))
	}
	post := func(body string) (bool, error) {
		stamp := strconv.FormatInt(now(), 10)
		return receiver.Receive(ctx, s, stamp, sign(stamp, body), []byte(body))
	}
	const body = `{"event_id":"ev-1","job_id":"job-ID"}`
	stamp := strconv.FormatInt(now(), 10)
	if _, err = receiver.Receive(ctx, s, stamp, "bad", []byte(body)); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("unsigned event accepted: %v", err)
	}
	old := strconv.FormatInt(now()-300001, 10)
	if _, err = receiver.Receive(ctx, s, old, sign(old, body), []byte(body)); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("stale signature accepted: %v", err)
	}
	if dup, err := post(body); err != nil || dup {
		t.Fatalf("early hint: %v %v", dup, err)
	}
	if dup, err := post(body); err != nil || !dup {
		t.Fatalf("duplicate hint: %v %v", dup, err)
	}
	if _, err = post(`{"event_id":"ev-1","job_id":"another"}`); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting hint accepted: %v", err)
	}
	lookup := func(want string) {
		t.Helper()
		var state string
		if err = s.db.QueryRow("SELECT state FROM inbox_events WHERE event_id='ev-1'").Scan(&state); err != nil || state != want {
			t.Fatalf("state = %q, %v, want %q", state, err, want)
		}
	}
	if err = s.applyBenchmarkHints(ctx, now()); err != nil {
		t.Fatal(err)
	}
	lookup("received")
	if provider.started != 0 || provider.read != 0 {
		t.Fatal("hint invoked provider")
	}
	if worked, err := s.runBenchmarkOnce(ctx, provider); !worked || err != nil {
		t.Fatalf("start %v %v", worked, err)
	}
	if _, err = s.db.Exec("UPDATE benchmark_cases SET next_at=?,call_until=? WHERE task_id=?", now()+60000, now()+60000, task); err != nil {
		t.Fatal(err)
	}
	if err = s.applyBenchmarkHints(ctx, now()); err != nil {
		t.Fatal(err)
	}
	lookup("received") // cannot preempt an active Get.
	if _, err = s.db.Exec("UPDATE benchmark_cases SET call_until=0 WHERE task_id=?", task); err != nil {
		t.Fatal(err)
	}
	if err = s.applyBenchmarkHints(ctx, now()); err != nil {
		t.Fatal(err)
	}
	lookup("applied")
	pg, err := s.BenchmarkProgress(ctx, actor, task)
	if err != nil || pg.Cases[0].Phase != "following" {
		t.Fatalf("hint falsely completed job: %+v %v", pg, err)
	}
	if worked, err := s.runBenchmarkOnce(ctx, provider); !worked || err != nil {
		t.Fatalf("read %v %v", worked, err)
	}
	if provider.started != 1 || provider.read != 1 {
		t.Fatalf("unexpected provider calls %+v", provider)
	}
}
