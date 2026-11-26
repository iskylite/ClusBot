package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"clusbot/internal/httpapi"
	"clusbot/internal/model"
	"clusbot/internal/work"
)

// The instance database owns credentials and configuration. Business data and
// files use an account-specific directory, so every query and worker has the
// same isolation boundary, including recovery and model inputs.
type accountRuntime struct {
	ctx                     context.Context
	cancel                  context.CancelFunc
	root                    string
	cfg                     config
	api                     httpapi.Server
	mu                      sync.Mutex
	handlers                map[string]http.Handler
	stores                  map[string]*work.Store
	background              sync.WaitGroup
	discovery               *work.HTTPDiscoverySource
	benchmark               *work.HTTPBenchmarkProvider
	cluskit                 *work.CluskitBenchmarkRuntime
	stopBenchmarkTransports []func()
}

func validateAccountBenchmarkConfig(cfg config) error {
	if cfg.Benchmark.EventSecretEnv != "" {
		return errors.New("账号服务不开放 benchmark.event_secret_env 回调入口；基准任务通过固定提供方查询原作业")
	}
	_, _, err := configuredAccountBenchmarkTransports(cfg)
	return err
}

func configuredAccountBenchmarkTransports(cfg config) (*work.HTTPBenchmarkProvider, *work.CluskitBenchmarkRuntime, error) {
	native := cfg.Benchmark.CluskitStdio
	if native.Command == "" && len(native.Environment) == 0 && len(native.ToolDigests) == 0 {
		http, err := configuredAccountBenchmarkProvider(cfg)
		return http, nil, err
	}
	if !cfg.Benchmark.EnableDispatch || cfg.Benchmark.BaseURL != "" || cfg.Benchmark.CredentialEnv != "" || len(cfg.Benchmark.Tools) != 0 {
		return nil, nil, errors.New("benchmark.cluskit_stdio 要求显式 enable_dispatch，且不能同时配置 HTTP 提供方")
	}
	provider, err := work.PrepareCluskitBenchmarkRuntime(work.CluskitBenchmarkStdio{
		Command: native.Command, Connection: cfg.Benchmark.Connection, Revision: cfg.Benchmark.Revision,
		Environment: native.Environment, ToolDigests: native.ToolDigests,
	})
	return nil, provider, err
}

func configuredAccountBenchmarkProvider(cfg config) (*work.HTTPBenchmarkProvider, error) {
	b := cfg.Benchmark
	if b.BaseURL == "" {
		if b.EnableDispatch {
			return nil, errors.New("benchmark.enable_dispatch 需要固定的 benchmark.base_url、连接配置和凭据")
		}
		return nil, nil
	}
	adapter, err := work.NewHTTPBenchmarkProvider(b.BaseURL, b.Connection, b.Revision, b.Tools, os.Getenv(b.CredentialEnv))
	if err != nil {
		return nil, err
	}
	if b.EnableDispatch {
		return adapter.ForAccountDispatch()
	}
	return adapter, nil
}

func startAccountRuntime(ctx context.Context, api *httpapi.Server, cfg config) (func(), error) {
	if cfg.Benchmark.EventSecretEnv != "" {
		return nil, validateAccountBenchmarkConfig(cfg)
	}
	benchmark, cluskit, err := configuredAccountBenchmarkTransports(cfg)
	if err != nil {
		return nil, err
	}
	if cluskit != nil {
		if err := cluskit.VerifyEndpoint(ctx); err != nil {
			return nil, err
		}
	}
	if err := api.Store.ConfigureCluskitBenchmarkIsolation(ctx, cluskit != nil); err != nil {
		return nil, err
	}
	key, err := connectionCredentialKey(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	api.Store.SetConnectionCredentialKey(key)
	api.Settings = &model.Settings{
		Store: api.Store, BaseURLOverride: strings.TrimSpace(os.Getenv("CLUSBOT_MODEL_BASE_URL")),
		ModelIDOverride: strings.TrimSpace(os.Getenv("CLUSBOT_MODEL_ID")), MaxRequestBytes: cfg.Runtime.MaxModelRequestBytes,
	}
	if _, err := api.Settings.Initialize(ctx, work.ModelSettings{
		Protocol: cfg.Model.Provider, BaseURL: cfg.Model.BaseURL, ModelID: cfg.Model.ID, KeyRef: cfg.Model.KeyRef,
	}); err != nil {
		return nil, err
	}
	api.ModelConcurrency = cfg.Runtime.MaxActiveRuns
	api.Store.ConfigureAccountModelConcurrency(cfg.Runtime.MaxActiveRuns)
	root := filepath.Join(cfg.DataDir, "account-data")
	if err := accountDirectory(root); err != nil {
		return nil, err
	}
	rows, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	businessCtx, cancel := context.WithCancel(ctx)
	api.MaxPendingExplicitTasks = cfg.Runtime.MaxPendingExplicitTasks
	runtime := &accountRuntime{
		ctx: businessCtx, cancel: cancel, root: root, cfg: cfg, api: *api,
		handlers: make(map[string]http.Handler), stores: make(map[string]*work.Store),
		benchmark: benchmark, cluskit: cluskit,
	}
	if cfg.Discovery.BaseURL != "" {
		runtime.discovery, err = work.NewHTTPDiscoverySource(cfg.Discovery.BaseURL, cfg.Discovery.Source, cfg.Discovery.Scopes, os.Getenv(cfg.Discovery.CredentialEnv))
		if err != nil {
			runtime.close()
			return nil, err
		}
		api.DiscoverySource = runtime.discovery
		runtime.api.DiscoverySource = runtime.discovery
	}
	for _, entry := range rows {
		if !entry.IsDir() || !validAccountDirectory(entry.Name()) {
			runtime.close()
			return nil, fmt.Errorf("个人数据目录含有无效项 %q", entry.Name())
		}
		principal := "user:" + entry.Name()
		exists, err := api.Store.AccountExists(ctx, principal)
		if err == nil && !exists {
			err = errors.New("个人数据目录没有对应账号")
		}
		if err == nil {
			_, err = runtime.handler(principal)
		}
		if err != nil {
			runtime.close()
			return nil, err
		}
	}
	api.PersonalAPI = runtime.handler
	api.AdminTaskStores = runtime.taskStores
	api.AccountBenchmarkDispatch = cfg.Benchmark.EnableDispatch
	return runtime.close, nil
}

func validAccountDirectory(name string) bool {
	return len(name) == 32 && strings.Trim(name, "0123456789abcdef") == ""
}

func accountDirectory(path string) error {
	if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("个人数据路径必须是目录")
	}
	return nil
}

func (a *accountRuntime) handler(principal string) (http.Handler, error) {
	name, ok := strings.CutPrefix(principal, "user:")
	if !ok || !validAccountDirectory(name) {
		return nil, errors.New("无效的账号标识")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.ctx.Err(); err != nil {
		return nil, err
	}
	if handler := a.handlers[principal]; handler != nil {
		return handler, nil
	}
	dir := filepath.Join(a.root, name)
	if err := accountDirectory(dir); err != nil {
		return nil, err
	}
	if file, err := os.Lstat(filepath.Join(dir, "clusbot.db")); err == nil && !file.Mode().IsRegular() {
		return nil, errors.New("个人数据库路径必须是普通文件")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	db, err := work.OpenAccountBusinessStore(filepath.Join(dir, "clusbot.db"), a.api.Store)
	if err != nil {
		return nil, err
	}
	opened := false
	defer func() {
		if !opened {
			db.Close()
		}
	}()
	if err := db.EnsureAccountMode(a.ctx, true); err != nil {
		return nil, err
	}
	if err := db.BindAccountBusinessStore(a.ctx, principal); err != nil {
		return nil, err
	}
	if a.benchmark != nil {
		if err := db.InstallBenchmarkProvider(a.ctx, a.benchmark); err != nil {
			return nil, err
		}
	}
	var provider work.BenchmarkProvider
	if a.benchmark != nil {
		provider = a.benchmark
	}
	var stopBenchmark func()
	if a.cluskit != nil {
		cluskit, stop, err := a.cluskit.NewAccountProvider()
		if err != nil {
			return nil, err
		}
		stopBenchmark = stop
		defer func() {
			if !opened {
				stopBenchmark()
			}
		}()
		if err := db.InstallCluskitBenchmarkProvider(a.ctx, cluskit); err != nil {
			return nil, err
		}
		provider = cluskit
	}
	if err := accountDirectory(filepath.Join(dir, "workspace")); err != nil {
		return nil, err
	}
	if err := accountDirectory(filepath.Join(dir, "uploads")); err != nil {
		return nil, err
	}
	if err := db.OpenWorkspace(filepath.Join(dir, "workspace")); err != nil {
		return nil, err
	}
	if err := db.OpenUploads(filepath.Join(dir, "uploads")); err != nil {
		return nil, err
	}
	if err := db.RecoverPublications(); err != nil {
		return nil, err
	}
	if err := db.Recover(); err != nil {
		return nil, err
	}
	if _, err := db.RecoverRuntime(a.ctx, time.Now().UnixMilli()); err != nil {
		return nil, err
	}
	if err := db.ExpireApprovals(a.ctx); err != nil {
		return nil, err
	}

	executor, err := db.EnableAssetAcceptanceQuery(a.ctx)
	if err != nil {
		return nil, err
	}
	if a.discovery != nil {
		if err := db.InstallDiscoverySource(a.ctx, a.discovery); err != nil {
			return nil, err
		}
	}
	// The instance owns connection configuration and usable credentials.
	// Published plans and observations still belong to the account database.
	accountDiscovery := work.GuardDiscoverySource(a.api.Store.DynamicDiscoverySource(a.discovery), func(ctx context.Context) error {
		allowed, err := a.api.Store.AccountCanRunDiscovery(ctx, principal)
		if err != nil {
			return err
		}
		if !allowed {
			return work.ErrAuthorizationDenied
		}
		return nil
	})

	accountActive := func() bool { return a.api.Store.AccountIsActive(a.ctx, principal) }
	loop := work.NewRuntimeLoop(db, nil)
	a.background.Go(func() { loop.Run(a.ctx, 250*time.Millisecond, 0) })
	a.background.Go(func() { executor.Run(a.ctx, 100*time.Millisecond) })
	a.background.Go(func() { db.RunDiscovery(a.ctx, accountDiscovery) })
	limits := work.RunLimits{
		ActiveTime:               time.Duration(a.cfg.Runtime.MaxRunSeconds) * time.Second,
		ModelTimeout:             time.Duration(a.cfg.Runtime.ModelTimeoutSeconds) * time.Second,
		OutputTokens:             a.cfg.Runtime.MaxOutputTokens,
		ModelRunPriorityFairness: a.cfg.Runtime.ModelRunPriorityFairness,
	}
	workers := db.StartWithModelRoles(a.ctx, a.api.Settings, a.cfg.Runtime.MaxActiveRuns, limits, accountActive, a.api.Settings.RoleAvailable)
	approvals := db.StartApprovalWorker(a.ctx)
	a.background.Go(workers.Wait)
	a.background.Go(approvals.Wait)
	a.background.Go(func() {
		db.RunChatAdmissions(a.ctx, accountActive, a.api.Settings.RouteSubmission, a.api.Settings.RoleAvailable)
	})
	accountAPI := a.api
	accountAPI.DiscoverySource = accountDiscovery
	if a.cfg.Benchmark.EnableDispatch {
		accountAPI.BenchmarkProvider = provider
		accountAPI.AccountBenchmarkDispatch = true
		a.background.Go(func() { db.RunBenchmarks(a.ctx, provider) })
	}
	handler := accountAPI.PersonalHandler(db)
	if a.benchmark != nil && !a.cfg.Benchmark.EnableDispatch {
		// These already-authorized account routes need the pinned adapter to
		// inspect and revise existing runs. Do not expose it to admission or
		// dispatch routes when the explicit instance switch is off.
		benchmarkAPI := accountAPI
		benchmarkAPI.BenchmarkProvider = a.benchmark
		benchmarkHandler := benchmarkAPI.PersonalHandler(db)
		mux := http.NewServeMux()
		for _, route := range []string{
			"GET /api/v1/domain/benchmarks/{id}",
			"POST /api/v1/conversations/{id}/benchmark-revision-preview",
			"GET /api/v1/conversations/{id}/benchmark-revision-preview/{run_id}",
			"POST /api/v1/conversations/{id}/benchmark-revision-preview/{run_id}/confirm",
		} {
			mux.Handle(route, benchmarkHandler)
		}
		mux.Handle("/", handler)
		handler = mux
	}
	a.handlers[principal] = handler
	a.stores[principal] = db
	if stopBenchmark != nil {
		a.stopBenchmarkTransports = append(a.stopBenchmarkTransports, stopBenchmark)
	}
	opened = true
	return handler, nil
}

func (a *accountRuntime) close() {
	a.cancel()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.background.Wait()
	for _, stop := range a.stopBenchmarkTransports {
		stop()
	}
	for _, db := range a.stores {
		db.Close()
	}
}

func (a *accountRuntime) taskStores() map[string]*work.Store {
	a.mu.Lock()
	defer a.mu.Unlock()
	return maps.Clone(a.stores)
}
