package work

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type mixedTicketTransport func(*http.Request) (*http.Response, error)

func (f mixedTicketTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// The production ticket loop takes a bounded snapshot before network I/O.
// Keep one HTTP lookup in flight while another loop and new admissions run.
func TestRuntimeTicketRecoveryDoesNotBlockBenchmarkOrBatchAdmissions(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	const recoveredKey = "mixed-ticket-original"
	var posts, originalPosts atomic.Int32
	lookupStarted := make(chan string, 1)
	lookupRelease := make(chan struct{})
	fake := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply := func(id string) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"ticket_id": id, "state": "open", "state_version": 1})
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/tickets":
			var request struct {
				Key string `json:"idempotency_key"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Key == "" {
				http.Error(w, "missing request key", http.StatusBadRequest)
				return
			}
			posts.Add(1)
			if request.Key == recoveredKey {
				originalPosts.Add(1)
				// The fake records the remote create but its accepted ID is
				// unavailable in the response.
				http.Error(w, "upstream response unavailable", http.StatusBadGateway)
				return
			}
			reply("ticket-" + request.Key)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tickets/by-request":
			key := r.URL.Query().Get("idempotency_key")
			if key != recoveredKey || originalPosts.Load() != 1 {
				http.Error(w, "unknown request", http.StatusNotFound)
				return
			}
			select {
			case lookupStarted <- key:
			case <-r.Context().Done():
				return
			}
			select {
			case <-lookupRelease:
				reply("ticket-" + key)
			case <-r.Context().Done():
			}
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/tickets/ticket-"):
			reply(strings.TrimPrefix(r.URL.Path, "/api/v1/tickets/"))
		default:
			http.NotFound(w, r)
		}
	})
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	provider, err := NewTicketProvider("http://127.0.0.1", "ticket-system", "")
	must(err)
	// Exercise the real provider's HTTP encoding and response handling with
	// an in-process transport, requiring no TCP listener or network access.
	provider.client.Transport = mixedTicketTransport(func(r *http.Request) (*http.Response, error) {
		if r.Body != nil {
			defer r.Body.Close()
		}
		w := httptest.NewRecorder()
		fake.ServeHTTP(w, r)
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		return w.Result(), nil
	})
	file, err := os.CreateTemp("", "clusbot-ticket-mixed-*.sqlite")
	must(err)
	path := file.Name()
	t.Cleanup(func() {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
				t.Errorf("remove test database: %v", err)
			}
		}
	})
	must(file.Close())
	s, err := Open(path)
	must(err)
	defer func() { s.Close() }()
	const actor, benchmarkTarget = "user:session", "mixed-ticket-benchmark"
	const arrivals = 9
	target := func(n int) string { return fmt.Sprintf("mixed-ticket-node-%d", n) }
	func() {
		tx, err := s.db.BeginTx(ctx, nil)
		must(err)
		defer tx.Rollback()
		stamp := now()
		for i := 0; i <= arrivals; i++ {
			ref := target(i)
			_, err := upsertAsset(tx, OpsAsset{ObjectRef: ref, Kind: "node", Identity: ref, State: AssetStateMaintenance, Revision: 1, OwnershipEpoch: 1}, stamp)
			must(err)
			_, err = grantAuthorization(tx, Authorization{Subject: actor, Action: "repair", ObjectRef: ref, ValidFrom: stamp, Owner: actor}, stamp)
			must(err)
		}
		_, err = grantAuthorization(tx, Authorization{Subject: actor, Action: "dispatch", ObjectRef: "ticket.create", ValidFrom: stamp, Owner: actor}, stamp)
		must(err)
		must(tx.Commit())
	}()
	enqueue := func(key, ref string) TicketCreateResult {
		t.Helper()
		r, err := s.QueueTicketCreate(ctx, TicketCreateRequest{RequestKey: key, ObjectRef: ref, Subject: actor, FaultEvidence: "mixed load fixture"}, provider)
		must(err)
		return r
	}
	state := func(id string) Operation {
		t.Helper()
		op, err := readOperation(s.db, id)
		must(err)
		return op
	}
	original := enqueue(recoveredKey, target(0))
	must(s.advanceTicketOperations(ctx, provider, nil))
	if o := state(original.OperationID); o.State != RuntimeOperationReconcile || o.AttemptCount != 1 || originalPosts.Load() != 1 {
		t.Fatalf("accepted create must be retained for reconciliation: %+v", o)
	}
	bench := &benchmarkRestartLoadFake{byKey: map[string]string{}, live: map[string]string{}, finished: map[string]bool{}, lostKey: "sentinel-no-lost-benchmark"}
	bind := BenchmarkBinding{Connection: "bench-test", Tool: "test", Revision: 1, Validator: benchmarkValidatorV1}
	grant := DomainBenchmarkGrant{Connection: bind.Connection, Tool: bind.Tool, Revision: bind.Revision, ExpiresAt: now() + 600000}
	must(s.ImportDomainAssets(ctx, []DomainAssetEntry{{ObjectRef: benchmarkTarget, Kind: "node", Identity: benchmarkTarget, Benchmark: []DomainBenchmarkGrant{grant}}}))
	must(s.PublishBenchmarkProfile(ctx, actor, BenchmarkProfile{Key: "mixed-ticket-profile", Version: 1, Cases: []BenchmarkCase{{
		Key: "measure", Binding: bind, Targets: []string{benchmarkTarget}, RequiredMetrics: map[string]string{"rate": "GB/s"},
	}}}))
	task, err := s.SubmitBenchmarkRun(ctx, actor, BenchmarkRunRequest{"mixed-ticket-bench", "mixed-ticket-profile", 1}, bench)
	must(err)
	must(s.Close())
	reopened, err := Open(path)
	must(err)
	s = reopened
	_, err = s.RecoverRuntime(ctx, now())
	must(err)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()
	ticketTick := make(chan error, 1)
	wg.Go(func() { ticketTick <- s.advanceTicketOperations(ctx, provider, nil) })
	select {
	case key := <-lookupStarted:
		if key != recoveredKey {
			t.Fatalf("lookup used wrong persisted request key: %q", key)
		}
	case err := <-ticketTick:
		t.Fatalf("ticket tick finished without reconciliation: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for i := 1; i <= arrivals; i++ {
		enqueue(fmt.Sprintf("mixed-ticket-arrival-%d", i), target(i))
	}
	for range 2 {
		worked, err := s.runBenchmarkOnce(ctx, bench)
		must(err)
		if !worked {
			t.Fatal("independent benchmark did not progress")
		}
	}
	p, err := s.BenchmarkProgress(ctx, actor, task)
	must(err)
	if len(p.Cases) != 1 || p.Cases[0].Phase != "verified" || posts.Load() != 1 {
		t.Fatalf("benchmark should be verified before releasing the ticket lookup: %+v, posts=%d", p, posts.Load())
	}
	if o := state(original.OperationID); o.State != RuntimeOperationReconcile || o.AttemptCount != 1 {
		t.Fatalf("in-flight reconciliation lost its durable state: %+v", o)
	}
	close(lookupRelease)
	select {
	case err := <-ticketTick:
		must(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var externalID string
	must(s.db.QueryRowContext(ctx, "SELECT provider_job_id FROM operation_jobs WHERE operation_id=?", original.OperationID).Scan(&externalID))
	if externalID != "ticket-"+recoveredKey || posts.Load() != 1 {
		t.Fatalf("recovery must bind original job without redispatch or absorbing later admissions: job=%s posts=%d", externalID, posts.Load())
	}
	must(s.advanceTicketOperations(ctx, provider, nil))
	if got := posts.Load(); got != 9 {
		t.Fatalf("next production tick must advance at most eight already-queued writes: posts=%d", got)
	}
	must(s.advanceTicketOperations(ctx, provider, nil))
	if got := posts.Load(); got != arrivals+1 || originalPosts.Load() != 1 {
		t.Fatalf("finite arrivals did not drain exactly once: posts=%d original=%d", got, originalPosts.Load())
	}
	must(s.advanceTicketOperations(ctx, provider, nil))
	var knownJobs int
	must(s.db.QueryRowContext(ctx, "SELECT count(*) FROM operation_jobs j JOIN operations o USING(operation_id) WHERE o.capability_key='ticket.create' AND o.attempt_count=1").Scan(&knownJobs))
	if knownJobs != arrivals+1 || posts.Load() != arrivals+1 {
		t.Fatalf("expected one durable job and provider write per admission: jobs=%d posts=%d", knownJobs, posts.Load())
	}
}
