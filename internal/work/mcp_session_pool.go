package work

import (
	"context"
	"errors"
	"sync"

	"clusbot/internal/mcpclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type pooledMCPSession struct {
	binding     string
	revision    int
	ctx         context.Context
	cancel      context.CancelCauseFunc
	process     context.Context
	stopProcess context.CancelCauseFunc
	ready       chan struct{}
	done        chan struct{}
	callSlot    chan struct{}

	// The setup results are immutable after ready is closed; closeErr is
	// published via done. The remaining setup fields are protected by pool.mu.
	session      *mcp.ClientSession
	catalog      mcpclient.Catalog
	setupErr     error
	closeErr     error
	initializing bool
	waiters      int
}

// mcpSessionPool owns the stdio subprocess lifetime, not the first caller.
// Calling from a later step can reuse the same server, including async jobs
// that the provider would cancel if its stdin reached EOF between steps.
type mcpSessionPool struct {
	mu        sync.Mutex
	entries   map[string]*pooledMCPSession
	active    map[*pooledMCPSession]struct{}
	revisions map[string]int
	closed    bool
}

func newMCPSessionPool() *mcpSessionPool {
	return &mcpSessionPool{
		entries:   make(map[string]*pooledMCPSession),
		active:    make(map[*pooledMCPSession]struct{}),
		revisions: make(map[string]int),
	}
}

func (s *Store) acquireMCPSession(ctx context.Context, c Connection, binding string) (*pooledMCPSession, error) {
	if s.mcpPool == nil {
		return nil, mcpclient.ErrConnect
	}
	return s.mcpPool.acquire(ctx, c, binding)
}

func (p *mcpSessionPool) acquire(ctx context.Context, c Connection, binding string) (*pooledMCPSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, context.Canceled
	}
	p.invalidateLocked(c.ID, c.Revision)
	if c.Revision < p.revisions[c.ID] {
		p.mu.Unlock()
		return nil, ErrConnectionRevision
	}
	entry := p.entries[c.ID]
	if entry != nil && entry.binding != binding {
		p.mu.Unlock()
		return nil, mcpclient.ErrSchemaChanged
	}
	if entry == nil {
		lifetime, stop := context.WithCancelCause(context.Background())
		process, stopProcess := context.WithCancelCause(context.Background())
		entry = &pooledMCPSession{
			binding: binding, revision: c.Revision, ctx: lifetime, cancel: stop,
			process: process, stopProcess: stopProcess,
			ready: make(chan struct{}), done: make(chan struct{}), callSlot: make(chan struct{}, 1),
			initializing: true,
		}
		p.entries[c.ID] = entry
		p.active[entry] = struct{}{}
		go p.open(c, entry)
	}
	entry.waiters++
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		entry.waiters--
		if entry.initializing && entry.waiters == 0 && p.entries[c.ID] == entry {
			delete(p.entries, c.ID)
			entry.cancel(context.Canceled)
		}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-entry.ready:
	case <-entry.ctx.Done():
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case <-entry.ready:
		if entry.setupErr != nil {
			return nil, entry.setupErr
		}
	default:
	}
	if err := context.Cause(entry.ctx); err != nil {
		return nil, err
	}
	return entry, nil
}

func (p *mcpSessionPool) open(c Connection, entry *pooledMCPSession) {
	defer func() {
		p.mu.Lock()
		close(entry.done)
		delete(p.active, entry)
		p.mu.Unlock()
	}()
	setup, cancel := context.WithTimeout(entry.ctx, mcpclient.Timeout)
	stopSetup := context.AfterFunc(setup, func() { entry.stopProcess(context.Cause(setup)) })
	entry.session, entry.setupErr = mcpclient.ConnectWithLifetime(setup, entry.process, c.Config)
	if entry.setupErr == nil {
		entry.catalog, entry.setupErr = mcpclient.Discover(setup, entry.session)
		if setup.Err() != nil {
			entry.setupErr = context.Cause(setup)
		}
	}
	stopSetup()
	cancel()
	p.mu.Lock()
	entry.initializing = false
	if entry.setupErr != nil && p.entries[c.ID] == entry {
		delete(p.entries, c.ID)
	}
	close(entry.ready)
	p.mu.Unlock()
	if entry.setupErr != nil {
		entry.cancel(entry.setupErr)
	}
	<-entry.ctx.Done()
	cause := context.Cause(entry.ctx)
	if entry.session != nil {
		if errors.Is(cause, ErrConnectionRevision) && entry.setupErr == nil {
			// Revocation invalidates the queue immediately. A call that has
			// already crossed the dispatch edge can still settle normally;
			// the caller's deadline and exact revision protect its result.
			select {
			case entry.callSlot <- struct{}{}:
				<-entry.callSlot
			case <-entry.process.Done():
			}
		}
		entry.stopProcess(cause)
		entry.closeErr = entry.session.Close()
	} else {
		entry.stopProcess(cause)
	}
}

// beginCall queues behind the current request without tying up callers that
// are canceled. Revoking the lease stops queued requests; terminating the
// process also stops active I/O.
func (e *pooledMCPSession) beginCall(ctx context.Context) (context.Context, func(), error) {
	select {
	case e.callSlot <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case <-e.ctx.Done():
		return nil, nil, context.Cause(e.ctx)
	}
	if err := ctx.Err(); err != nil {
		<-e.callSlot
		return nil, nil, err
	}
	if err := context.Cause(e.ctx); err != nil {
		<-e.callSlot
		return nil, nil, err
	}
	callCtx, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(e.process, func() { cancel(context.Cause(e.process)) })
	return callCtx, func() {
		stop()
		cancel(nil)
		<-e.callSlot
	}, nil
}

func (p *mcpSessionPool) invalidateLocked(key string, revision int) {
	p.revisions[key] = max(revision, p.revisions[key])
	if entry := p.entries[key]; entry != nil && entry.revision < revision {
		delete(p.entries, key)
		entry.cancel(ErrConnectionRevision)
	}
}

func (s *Store) invalidateMCPSession(key string, revision int) {
	if s.mcpPool == nil || key == "" {
		return
	}
	s.mcpPool.mu.Lock()
	s.mcpPool.invalidateLocked(key, revision)
	s.mcpPool.mu.Unlock()
}

// A stale or failed call can retire only the exact session it used.
func (p *mcpSessionPool) retire(key string, entry *pooledMCPSession, cause error) {
	p.mu.Lock()
	if p.entries[key] == entry {
		delete(p.entries, key)
	}
	entry.cancel(cause)
	entry.stopProcess(cause)
	p.mu.Unlock()
}

func (p *mcpSessionPool) closeAll() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	p.closed = true
	clear(p.entries)
	entries := make([]*pooledMCPSession, 0, len(p.active))
	for entry := range p.active {
		entry.cancel(context.Canceled)
		entry.stopProcess(context.Canceled)
		entries = append(entries, entry)
	}
	p.mu.Unlock()
	var firstErr error
	for _, entry := range entries {
		<-entry.done
		if err := entry.closeErr; err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
