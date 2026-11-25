package work

import (
	"context"
	"sync"

	"clusbot/internal/mcpclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type pooledMCPSession struct {
	binding  string
	revision int
	session  *mcp.ClientSession
	cancel   context.CancelFunc
	catalog  mcpclient.Catalog
	callMu   sync.Mutex
}

// mcpSessionPool caches long-lived stdio MCP child processes keyed by connection ID.
// Entries are evicted whenever connection config, revision, or tool permissions change,
// or when a transport call returns an error.
type mcpSessionPool struct {
	mu      sync.Mutex
	entries map[string]*pooledMCPSession
}

func newMCPSessionPool() *mcpSessionPool {
	return &mcpSessionPool{entries: make(map[string]*pooledMCPSession)}
}

func (s *Store) acquireMCPSession(ctx context.Context, c Connection, binding string) (*pooledMCPSession, error) {
	if s.mcpPool == nil {
		session, err := mcpclient.Connect(ctx, c.Config)
		if err != nil {
			return nil, err
		}
		catalog, err := mcpclient.Discover(ctx, session)
		if err != nil {
			_ = session.Close()
			return nil, err
		}
		return &pooledMCPSession{binding: binding, revision: c.Revision, session: session, catalog: catalog}, nil
	}

	s.mcpPool.mu.Lock()
	existing := s.mcpPool.entries[c.ID]
	if existing != nil && existing.binding == binding && existing.revision == c.Revision {
		s.mcpPool.mu.Unlock()
		return existing, nil
	}
	if existing != nil {
		delete(s.mcpPool.entries, c.ID)
		_ = existing.session.Close()
		if existing.cancel != nil {
			existing.cancel()
		}
	}
	s.mcpPool.mu.Unlock()

	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	session, err := mcpclient.Connect(sessionCtx, c.Config)
	if err != nil {
		sessionCancel()
		return nil, err
	}
	catalog, err := mcpclient.Discover(ctx, session)
	if err != nil {
		_ = session.Close()
		sessionCancel()
		return nil, err
	}
	created := &pooledMCPSession{
		binding:  binding,
		revision: c.Revision,
		session:  session,
		cancel:   sessionCancel,
		catalog:  catalog,
	}

	s.mcpPool.mu.Lock()
	if current := s.mcpPool.entries[c.ID]; current != nil {
		if current.binding == binding && current.revision == c.Revision {
			s.mcpPool.mu.Unlock()
			_ = session.Close()
			sessionCancel()
			return current, nil
		}
		_ = current.session.Close()
		if current.cancel != nil {
			current.cancel()
		}
	}
	s.mcpPool.entries[c.ID] = created
	s.mcpPool.mu.Unlock()
	return created, nil
}

func (s *Store) evictMCPSession(key string) {
	if s.mcpPool == nil || key == "" {
		return
	}
	s.mcpPool.mu.Lock()
	existing := s.mcpPool.entries[key]
	delete(s.mcpPool.entries, key)
	s.mcpPool.mu.Unlock()
	if existing != nil {
		_ = existing.session.Close()
		if existing.cancel != nil {
			existing.cancel()
		}
	}
}

func (p *mcpSessionPool) closeAll() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	entries := make([]*pooledMCPSession, 0, len(p.entries))
	for key, entry := range p.entries {
		entries = append(entries, entry)
		delete(p.entries, key)
	}
	p.mu.Unlock()
	var firstErr error
	for _, entry := range entries {
		if err := entry.session.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		if entry.cancel != nil {
			entry.cancel()
		}
	}
	return firstErr
}
