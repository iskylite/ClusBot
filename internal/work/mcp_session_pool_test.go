package work

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"clusbot/internal/mcpclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPSessionPoolFixture(t *testing.T) {
	switch os.Getenv("CLUSBOT_TEST_MCP_POOL_MODE") {
	case "hung":
		// Not a shell or external service: this process never reads stdin or
		// replies, and should be reaped by the stdio transport.
		time.Sleep(30 * time.Second)
		os.Exit(3)
	case "slow":
		time.Sleep(400 * time.Millisecond)
	case "ready":
	default:
		t.Skip("used by the local stdio subprocess fixture")
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "pool fixture", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{
		Name: "pool_status", InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct{ Hold bool }
		_ = json.Unmarshal(req.Params.Arguments, &args)
		if args.Hold {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strconv.Itoa(os.Getpid())}}}, nil
	})
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func mcpPoolFixture(t *testing.T, mode string) Connection {
	t.Helper()
	t.Setenv("CLUSBOT_TEST_MCP_POOL_MODE", mode)
	// Race builds artificially hold os.Exit for a second by default,
	// longer than the production stdio graceful-close window.
	t.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0")
	cmd, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Connection{ID: "test-pool", Revision: 1, Config: mcpclient.Config{
		Transport: "stdio", Command: cmd,
		Args: []string{"-test.run=^TestMCPSessionPoolFixture$"},
		Env:  []string{"CLUSBOT_TEST_MCP_POOL_MODE", "GORACE"},
	}}
}

func waitPoolEntry(t *testing.T, p *mcpSessionPool, key string, waiters int) *pooledMCPSession {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		entry := p.entries[key]
		waiting := entry != nil && entry.initializing && entry.waiters == waiters
		p.mu.Unlock()
		if waiting {
			return entry
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("MCP session did not reach %d setup waiters", waiters)
	return nil
}

func receivePoolResult[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("MCP session operation did not finish")
		var zero T
		return zero
	}
}

func TestMCPStdioHandshakeDeadline(t *testing.T) {
	c := mcpPoolFixture(t, "hung")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		session, err := mcpclient.ConnectWithLifetime(ctx, context.Background(), c.Config)
		if session != nil {
			_ = session.Close()
		}
		result <- err
	}()
	if err := receivePoolResult(t, result); !errors.Is(err, mcpclient.ErrConnect) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hung handshake returned %v, want connection deadline", err)
	}
}

func TestMCPSessionPoolStopsUnneededHandshake(t *testing.T) {
	c := mcpPoolFixture(t, "hung")
	p := newMCPSessionPool()
	defer p.closeAll()
	for _, shutdown := range []bool{false, true} {
		t.Run(strconv.FormatBool(shutdown), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, err := p.acquire(ctx, c, "")
				result <- err
			}()
			e := waitPoolEntry(t, p, c.ID, 1)
			if shutdown {
				done := make(chan error, 1)
				go func() { done <- p.closeAll() }()
				_ = receivePoolResult(t, done)
			} else {
				cancel()
			}
			if err := receivePoolResult(t, result); !errors.Is(err, context.Canceled) {
				t.Fatalf("startup cancellation returned %v", err)
			}
			receivePoolResult(t, e.done)
		})
	}
}

func TestMCPSessionPoolSharesStartupAndPreservesProvider(t *testing.T) {
	c := mcpPoolFixture(t, "slow")
	p := newMCPSessionPool()
	defer p.closeAll()
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	first := make(chan error, 1)
	go func() {
		_, err := p.acquire(firstCtx, c, "")
		first <- err
	}()
	e := waitPoolEntry(t, p, c.ID, 1)
	type response struct {
		session *pooledMCPSession
		err     error
	}
	second := make(chan response, 1)
	go func() {
		session, err := p.acquire(context.Background(), c, "")
		second <- response{session, err}
	}()
	if waitPoolEntry(t, p, c.ID, 2) != e {
		t.Fatal("concurrent callers started two child processes")
	}
	cancelFirst()
	if err := receivePoolResult(t, first); !errors.Is(err, context.Canceled) {
		t.Fatalf("first waiting caller: %v", err)
	}
	other := receivePoolResult(t, second)
	if other.err != nil || other.session != e || len(e.catalog.Tools) != 1 {
		t.Fatalf("canceling one waiter affected other callers: %+v", other)
	}
	lookup := func(ctx context.Context) string {
		t.Helper()
		current, err := p.acquire(ctx, c, "")
		if err != nil || current != e {
			t.Fatalf("did not reuse provider: %v", err)
		}
		ioCtx, release, err := current.beginCall(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		result, _, err := mcpclient.Call(ioCtx, current.session, current.catalog.Tools[0], map[string]any{}, c.Config)
		if err != nil || result.Text == "" {
			t.Fatalf("provider no longer usable: %q, %v", result.Text, err)
		}
		return result.Text
	}
	callCtx, cancelCall := context.WithCancel(context.Background())
	defer cancelCall()
	pid := lookup(callCtx)
	cancelCall()
	if next := lookup(context.Background()); next != pid {
		t.Fatalf("successful call cancel closed stdio provider: PID changed %s to %s", pid, next)
	}
}

func TestMCPSessionPoolQueueReleasesAndInvalidatesExactRevision(t *testing.T) {
	c := mcpPoolFixture(t, "ready")
	p := newMCPSessionPool()
	t.Cleanup(func() { _ = p.closeAll() })
	e, err := p.acquire(context.Background(), c, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, release, err := e.beginCall(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	queuedCtx, cancelQueued := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelQueued()
	if _, _, err := e.beginCall(queuedCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued call failed to honor deadline: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("queued cancellation interrupted active call: %v", ctx.Err())
	}
	queued := make(chan error, 1)
	go func() {
		_, release, err := e.beginCall(context.Background())
		if release != nil {
			release()
		}
		queued <- err
	}()
	replacement := c
	replacement.Revision++
	next, err := p.acquire(context.Background(), replacement, "")
	if err != nil || next == e {
		t.Fatalf("new revision did not supersede the active entry: %v", err)
	}
	if err := receivePoolResult(t, queued); !errors.Is(err, ErrConnectionRevision) {
		t.Fatalf("stale queued call: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("revision change aborted a request already using that session: %v", ctx.Err())
	}
	if _, err := p.acquire(context.Background(), c, ""); !errors.Is(err, ErrConnectionRevision) {
		t.Fatalf("old revision was allowed to reconnect: %v", err)
	}
	p.retire(c.ID, e, mcpclient.ErrCall)
	receivePoolResult(t, ctx.Done())
	if reused, err := p.acquire(context.Background(), replacement, ""); err != nil || reused != next {
		t.Fatalf("a fault in the stale session evicted the new one: %v", err)
	}
	// Successful release admits the next same-revision call.
	nextCtx, cancelNext := context.WithTimeout(context.Background(), time.Second)
	defer cancelNext()
	_, unlockNext, err := next.beginCall(nextCtx)
	if err != nil {
		t.Fatal(err)
	}
	active := make(chan error, 1)
	go func() {
		ctx, release, err := next.beginCall(nextCtx)
		if err == nil {
			defer release()
			_, _, err = mcpclient.Call(ctx, next.session, next.catalog.Tools[0], map[string]any{}, c.Config)
		}
		active <- err
	}()
	unlockNext()
	if err := receivePoolResult(t, active); err != nil {
		t.Fatalf("released queue did not resume correctly: %v", err)
	}
}

func TestMCPSessionPoolCloseWakesActiveAndQueuedCalls(t *testing.T) {
	c := mcpPoolFixture(t, "ready")
	p := newMCPSessionPool()
	t.Cleanup(func() { _ = p.closeAll() })
	e, err := p.acquire(context.Background(), c, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, release, err := e.beginCall(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	active := make(chan error, 1)
	go func() {
		defer release()
		_, _, err := mcpclient.Call(ctx, e.session, e.catalog.Tools[0], map[string]any{"hold": true}, c.Config)
		active <- err
	}()
	queued := make(chan error, 1)
	go func() {
		_, release, err := e.beginCall(context.Background())
		if release != nil {
			release()
		}
		queued <- err
	}()
	done := make(chan error, 1)
	go func() { done <- p.closeAll() }()
	if err := receivePoolResult(t, queued); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown did not cancel the queue: %v", err)
	}
	if err := receivePoolResult(t, active); !errors.Is(err, mcpclient.ErrCall) {
		t.Fatalf("shutdown did not interrupt active RPC: %v", err)
	}
	if err := receivePoolResult(t, done); err != nil {
		t.Fatalf("stdio fixture did not exit cleanly on EOF: %v", err)
	}
	if _, err := p.acquire(context.Background(), c, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("closed pool accepted a new child: %v", err)
	}
}

func TestMCPSessionPoolCanceledQueueDoesNotDispatch(t *testing.T) {
	ctx := context.Background()
	fixture := mcpPoolFixture(t, "ready")
	s, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := s.SaveConnection(ctx, "", Connection{Name: "本地进程夹具", Enabled: true, Config: fixture.Config})
	if err != nil {
		t.Fatal(err)
	}
	c, err = s.TestConnection(ctx, c.ID, c.Revision)
	if err != nil || c.Status != "ready" || len(c.Tools) != 1 {
		t.Fatalf("cannot test fixture connection: %+v %v", c, err)
	}
	name := c.Tools[0].Name
	if err = s.SetToolPermission(ctx, c.ID, name, c.Tools[0].Version, c.Revision, true); err != nil {
		t.Fatal(err)
	}
	c, err = s.Connection(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.acquireMCPSession(ctx, c, c.credentialBinding)
	if err != nil {
		t.Fatal(err)
	}
	_, release, err := e.beginCall(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if release != nil {
			release()
		}
	}()
	deadline, cancel := context.WithTimeout(ctx, 120*time.Millisecond)
	defer cancel()
	receipt, err := s.CallConnectionTool(deadline, c.ID, name, c.Revision, map[string]any{})
	if !errors.Is(err, context.DeadlineExceeded) || receipt.ID != "" {
		t.Fatalf("queued timeout got a dispatch receipt: %+v, %v", receipt, err)
	}
	var calls int
	if err := s.db.QueryRow(`SELECT count(*) FROM connection_calls`).Scan(&calls); err != nil || calls != 0 {
		t.Fatalf("canceled queue left a durable call: count=%d %v", calls, err)
	}
	release()
	release = nil
	receipt, err = s.CallConnectionTool(ctx, c.ID, name, c.Revision, map[string]any{})
	if err != nil || receipt.ID == "" || receipt.Text == "" || receipt.SourceUnavailable {
		t.Fatalf("authorized call after queue cancellation: %+v %v", receipt, err)
	}
	reused, err := s.acquireMCPSession(ctx, c, c.credentialBinding)
	if err != nil || reused != e {
		t.Fatalf("queue cancellation needlessly restarted provider: %v", err)
	}
}
