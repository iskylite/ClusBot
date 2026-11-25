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
	ctx        context.Context
	cancel     context.CancelFunc
	root       string
	cfg        config
	api        httpapi.Server
	mu         sync.Mutex
	handlers   map[string]http.Handler
	stores     map[string]*work.Store
	background sync.WaitGroup
	discovery  *work.HTTPDiscoverySource
	benchmark  *work.HTTPBenchmarkProvider
}

func startAccountRuntime(ctx context.Context, api *httpapi.Server, cfg config) (func(), error) {
	key, err := accessToken(filepath.Join(cfg.DataDir, "connection-key"))
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
	}
	if cfg.desktop && cfg.Benchmark.BaseURL != "" {
		runtime.benchmark, err = work.NewHTTPBenchmarkProvider(cfg.Benchmark.BaseURL, cfg.Benchmark.Connection,
			cfg.Benchmark.Revision, cfg.Benchmark.Tools, os.Getenv(cfg.Benchmark.CredentialEnv))
		if err != nil {
			runtime.close()
			return nil, err
		}
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
	accountDiscovery := db.DynamicDiscoverySource(a.discovery)

	available := func() bool {
		return a.api.ModelAvailable(a.ctx) && a.api.Store.AccountIsActive(a.ctx, principal)
	}
	loop := work.NewRuntimeLoop(db, available)
	a.background.Go(func() { loop.Run(a.ctx, 250*time.Millisecond, 0) })
	a.background.Go(func() { executor.Run(a.ctx, 100*time.Millisecond) })
	source := accountDiscoverySource{accountDiscovery, a.api.Store, principal}
	a.background.Go(func() { db.RunDiscovery(a.ctx, source) })
	limits := work.RunLimits{
		ActiveTime:               time.Duration(a.cfg.Runtime.MaxRunSeconds) * time.Second,
		ModelTimeout:             time.Duration(a.cfg.Runtime.ModelTimeoutSeconds) * time.Second,
		OutputTokens:             a.cfg.Runtime.MaxOutputTokens,
		ModelRunPriorityFairness: a.cfg.Runtime.ModelRunPriorityFairness,
	}
	workers := db.StartWhenModelAvailable(a.ctx, a.api.Settings, a.cfg.Runtime.MaxActiveRuns, limits, available)
	approvals := db.StartApprovalWorker(a.ctx)
	a.background.Go(workers.Wait)
	a.background.Go(approvals.Wait)
	a.background.Go(func() { db.RunChatAdmissions(a.ctx, available, a.api.Settings.RouteSubmission) })
	handler := a.api.PersonalHandler(db)
	if a.benchmark != nil {
		// These already-authorized account routes need the pinned adapter to
		// inspect and revise existing runs. Do not expose it to admission or
		// dispatch routes, and do not start an account benchmark worker.
		benchmarkAPI := a.api
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
	opened = true
	return handler, nil
}

func (a *accountRuntime) close() {
	a.cancel()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.background.Wait()
	for _, db := range a.stores {
		db.Close()
	}
}

func (a *accountRuntime) taskStores() map[string]*work.Store {
	a.mu.Lock()
	defer a.mu.Unlock()
	return maps.Clone(a.stores)
}

// Recheck the instance account at dispatch so disabling or demoting an account
// also removes its ability to run previously authorized discovery plans.
type accountDiscoverySource struct {
	work.DiscoveryScanSource
	accounts  *work.Store
	principal string
}

func (s accountDiscoverySource) ScanDiscovery(ctx context.Context, source, scope string) ([]work.DiscoveryObservation, error) {
	allowed, err := s.accounts.AccountCanRunDiscovery(ctx, s.principal)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, work.ErrAuthorizationDenied
	}
	return s.DiscoveryScanSource.ScanDiscovery(ctx, source, scope)
}
