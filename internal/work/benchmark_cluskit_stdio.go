package work

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"clusbot/internal/mcpclient"
)

// CluskitBenchmarkStdio is an operator-configured, runtime-only transport.
// It is intentionally not a shared connection or model-selectable tool.
type CluskitBenchmarkStdio struct {
	Command     string
	Connection  string
	Revision    int
	Environment []string
	ToolDigests map[string]string
}

// ConfigureCluskitBenchmarkIsolation is part of account runtime setup, before
// handlers or workers start. Even if dispatch is disabled on a later start,
// archived operations may still exist in the protected provider namespace.
func (s *Store) ConfigureCluskitBenchmarkIsolation(ctx context.Context, protect bool) error {
	if protect {
		if _, err := s.db.ExecContext(ctx, "INSERT OR IGNORE INTO benchmark_account_cluskit_isolation(singleton) VALUES(1)"); err != nil {
			return err
		}
	}
	return s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM benchmark_account_cluskit_isolation)").Scan(&s.reserveCluskitBenchmarkTools)
}

func (s *Store) cluskitBenchmarkToolReserved(name string) bool {
	if !s.reserveCluskitBenchmarkTools {
		return false
	}
	switch name {
	case "bench_run", "task_stream", "task_status", "task_list", "task_topo", "task_cancel", "task_artifacts", "task_recover":
		return true
	default:
		return false
	}
}

type cluskitPinnedPath struct {
	requested, resolved string
	info                os.FileInfo
	native              *syscall.Stat_t
}

func pinCluskitPath(input string, dir bool) (cluskitPinnedPath, error) {
	var p cluskitPinnedPath
	if !filepath.IsAbs(input) {
		return p, ErrBenchmarkBinding
	}
	resolved, err := filepath.EvalSymlinks(input)
	if err != nil {
		return p, ErrBenchmarkBinding
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return p, ErrBenchmarkBinding
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.IsDir() != dir || !dir && !info.Mode().IsRegular() ||
		info.Mode().Perm()&0022 != 0 || st.Uid != 0 && uint64(st.Uid) != uint64(os.Geteuid()) {
		return p, ErrBenchmarkBinding
	}
	return cluskitPinnedPath{requested: input, resolved: resolved, info: info, native: st}, nil
}

func (p cluskitPinnedPath) identity() []any {
	return []any{p.resolved, p.native.Dev, p.native.Ino, p.native.Uid}
}

func (p cluskitPinnedPath) unchanged() bool {
	current, err := pinCluskitPath(p.requested, p.info.IsDir())
	if err != nil || p.resolved != current.resolved || !os.SameFile(p.info, current.info) ||
		p.info.Mode() != current.info.Mode() || p.native.Uid != current.native.Uid {
		return false
	}
	return p.info.IsDir() || p.info.Size() == current.info.Size() && p.info.ModTime().Equal(current.info.ModTime())
}

// A validated configuration is only a blueprint. It does not start a process
// and can create independently supervised sessions for different accounts.
type CluskitBenchmarkRuntime struct {
	command, config, work, tasks, logs cluskitPinnedPath
	conn                               Connection
	jobSource, pin, envBinding         string
	envKey                             []byte
	tools                              map[string]string
}

func PrepareCluskitBenchmarkRuntime(in CluskitBenchmarkStdio) (*CluskitBenchmarkRuntime, error) {
	invalid := func(field string) (*CluskitBenchmarkRuntime, error) {
		return nil, fmt.Errorf("%w：Cluskit 原生基准配置 %s 不可用", ErrBenchmarkBinding, field)
	}
	if !validBenchKey(in.Connection) || in.Revision < 1 {
		return invalid("connection/revision")
	}
	command, err := pinCluskitPath(in.Command, false)
	if err != nil || command.info.Mode().Perm()&0100 == 0 || command.info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		return invalid("command")
	}
	work, err := pinCluskitPath(os.Getenv("CLUS_ROOT"), true)
	if err != nil {
		return invalid("CLUS_ROOT")
	}
	tasks, err := pinCluskitPath(os.Getenv("CLUS_TASK_DIR"), true)
	if err != nil {
		return invalid("CLUS_TASK_DIR")
	}
	logs, err := pinCluskitPath(os.Getenv("CLUSKIT_LOG_DIR"), true)
	if err != nil {
		return invalid("CLUSKIT_LOG_DIR")
	}
	config, err := pinCluskitPath(os.Getenv("CLUS_CONFIG_FILE"), false)
	if err != nil {
		return invalid("CLUS_CONFIG_FILE")
	}
	tools := make(map[string]string, 3)
	for _, name := range []string{"bench_run", "task_status", "task_cancel"} {
		raw, err := hex.DecodeString(in.ToolDigests[name])
		if err != nil || len(raw) != 32 || len(in.ToolDigests) != 3 {
			return invalid("tool_digests")
		}
		tools[name] = hex.EncodeToString(raw)
	}
	// Only these optional names are inherited. Credentials and installation
	// paths come from the operator's fixed configuration, not the model.
	env := []string{"CLUS_ROOT", "CLUS_TASK_DIR", "CLUSKIT_LOG_DIR", "CLUS_CONFIG_FILE"}
	for _, name := range in.Environment {
		switch name {
		case "HOME", "PATH", "LANG", "LC_ALL":
			if slices.Contains(env, name) {
				return invalid("environment")
			}
			env = append(env, name)
		default:
			return invalid("environment")
		}
	}
	sortEnv := slices.Clone(env)
	slices.Sort(sortEnv)
	mcpConfig := mcpclient.Config{Transport: "stdio", Command: command.resolved, Args: []string{"mcp"}, Env: sortEnv}
	if err := mcpConfig.Validate(); err != nil {
		return invalid("command/environment")
	}
	envKey := []byte("clusbot-cluster-stdio-environment")
	envBinding, err := mcpConfig.CredentialBinding(envKey)
	if err != nil {
		return invalid("environment")
	}
	source := digest(toJSON([]any{"cluskit-benchmark", tasks.identity(), os.Geteuid()}))
	pin := digest(toJSON([]any{source, in.Connection, in.Revision, command.identity(), command.info.Size(),
		command.info.ModTime().UnixNano(), work.identity(), logs.identity(), config.identity(), config.info.Size(),
		config.info.ModTime().UnixNano(), envBinding, tools}))
	return &CluskitBenchmarkRuntime{
		command: command, config: config, work: work, tasks: tasks, logs: logs,
		conn:      Connection{ID: "runtime-cluskit-benchmark", Name: in.Connection, Revision: in.Revision, Config: mcpConfig},
		jobSource: source, pin: pin, envBinding: envBinding, envKey: envKey, tools: tools,
	}, nil
}

func (r *CluskitBenchmarkRuntime) current() bool {
	if r == nil || !r.command.unchanged() || !r.work.unchanged() || !r.tasks.unchanged() ||
		!r.logs.unchanged() || !r.config.unchanged() {
		return false
	}
	env, err := r.conn.Config.CredentialBinding(r.envKey)
	return err == nil && env == r.envBinding
}

func (r *CluskitBenchmarkRuntime) verifyCatalog(ctx context.Context, entry *pooledMCPSession) (map[string]mcpclient.Tool, error) {
	init := entry.session.InitializeResult()
	if !r.current() || init == nil || init.ServerInfo == nil || !strings.EqualFold(init.ServerInfo.Name, "clusbench") {
		return nil, ErrBenchmarkBinding
	}
	catalog, err := mcpclient.Discover(ctx, entry.session)
	if err != nil || catalog.Protocol != entry.catalog.Protocol {
		return nil, ErrBenchmarkBinding
	}
	verified := make(map[string]mcpclient.Tool, len(r.tools))
	for _, tool := range catalog.Tools {
		expected, allowed := r.tools[tool.Name]
		if !allowed {
			continue
		}
		if expected != tool.Version || tool.ServerReadOnly == nil ||
			*tool.ServerReadOnly != (tool.Name == "task_status") ||
			mcpclient.ValidateToolSchemas(tool) != nil {
			return nil, ErrBenchmarkBinding
		}
		verified[tool.Name] = tool
	}
	if len(verified) != len(r.tools) {
		return nil, ErrBenchmarkBinding
	}
	return verified, nil
}

// VerifyEndpoint is an opt-in startup probe. It checks the actual executing
// identity/catalog before accepting requests, without calling any tool.
func (r *CluskitBenchmarkRuntime) VerifyEndpoint(ctx context.Context) error {
	if !r.current() {
		return ErrBenchmarkBinding
	}
	ctx, cancel := context.WithTimeout(ctx, mcpclient.Timeout)
	defer cancel()
	pool := newMCPSessionPool()
	defer pool.closeAll()
	entry, err := pool.acquire(ctx, r.conn, r.envBinding)
	if err != nil {
		return fmt.Errorf("%w：Cluskit 基准进程或当前执行身份不可用", ErrBenchmarkBinding)
	}
	if _, err := r.verifyCatalog(ctx, entry); err != nil {
		return fmt.Errorf("%w：Cluskit 未提供全部已批准的基准工具", ErrBenchmarkBinding)
	}
	return nil
}

func (r *CluskitBenchmarkRuntime) NewAccountProvider() (*MCPBenchmarkProvider, func(), error) {
	if !r.current() {
		return nil, nil, ErrBenchmarkBinding
	}
	pool := newMCPSessionPool()
	stop := func() { _ = pool.closeAll() }
	caller := func(ctx context.Context, name string, args map[string]any) (mcpclient.Result, error) {
		if !r.current() || r.tools[name] == "" {
			return mcpclient.Result{}, ErrBenchmarkBinding
		}
		entry, err := pool.acquire(ctx, r.conn, r.envBinding)
		if err != nil {
			return mcpclient.Result{}, err
		}
		callCtx, release, err := entry.beginCall(ctx)
		if err != nil {
			return mcpclient.Result{}, err
		}
		defer release()
		tools, err := r.verifyCatalog(callCtx, entry)
		if err != nil {
			pool.retire(r.conn.ID, entry, ErrBenchmarkBinding)
			return mcpclient.Result{}, ErrBenchmarkBinding
		}
		selected := tools[name]
		if err := mcpclient.ValidateArguments(selected, args); err != nil {
			return mcpclient.Result{}, err
		}
		abort := context.AfterFunc(callCtx, func() { pool.retire(r.conn.ID, entry, context.Cause(callCtx)) })
		result, evidence, err := mcpclient.Call(callCtx, entry.session, selected, args, r.conn.Config)
		abort()
		if err != nil {
			pool.retire(r.conn.ID, entry, err)
			return mcpclient.Result{}, err
		}
		if result.Truncated && len(evidence) != 0 {
			if err := json.Unmarshal(evidence, &result); err != nil {
				return mcpclient.Result{}, err
			}
		}
		if result.Truncated {
			return mcpclient.Result{}, ErrBenchmarkBinding
		}
		return result, nil
	}
	provider, err := NewMCPBenchmarkProvider(r.conn.Name, r.conn.Revision,
		MCPToolBinding{StartTool: "bench_run", LookupTool: "task_status", GetTool: "task_status", CancelTool: "task_cancel"}, caller)
	if err != nil {
		stop()
		return nil, nil, err
	}
	provider.pin, provider.jobSource, provider.runtimeReady = r.pin, r.jobSource, r.current
	return provider, stop, nil
}
