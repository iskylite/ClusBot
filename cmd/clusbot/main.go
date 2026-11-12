package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"clusbot/internal/httpapi"
	"clusbot/internal/model"
	"clusbot/internal/work"
	"clusbot/web"

	"gopkg.in/yaml.v3"
)

type config struct {
	Server        struct{ Listen string } `yaml:"server"`
	DataDir       string                  `yaml:"data_dir"`
	WorkspaceRoot string                  `yaml:"workspace_root"`
	Model         struct {
		Provider string `yaml:"provider"`
		BaseURL  string `yaml:"base_url"`
		ID       string `yaml:"model_id"`
		KeyRef   string `yaml:"api_key_ref"`
	} `yaml:"model"`
	Runtime struct {
		MaxActiveRuns        int `yaml:"max_active_runs"`
		MaxRunSeconds        int `yaml:"max_run_seconds"`
		ModelTimeoutSeconds  int `yaml:"model_timeout_seconds"`
		MaxOutputTokens      int `yaml:"max_output_tokens"`
		MaxModelRequestBytes int `yaml:"max_model_request_bytes"`
	} `yaml:"runtime"`
}

func main() {
	if err := run(); err != nil {
		slog.Error("启动失败", "error", err)
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "backup":
			return runBackup(args[1:])
		case "restore":
			return runRestore(args[1:])
		case "evaluate-experience":
			return runExperienceEvaluation(args[1:])
		}
	}
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	flags := flag.NewFlagSet("clusbot serve", flag.ContinueOnError)
	configPath := flags.String("config", "", "YAML 配置文件")
	listen := flags.String("listen", "", "覆盖监听地址")
	dataDir := flags.String("data-dir", "", "覆盖数据目录")
	workspaceRoot := flags.String("workspace-root", "", "工作区目录")
	printURL := flags.Bool("print-url", false, "服务就绪后向标准输出写入本机 URL")
	demo := flags.Bool("demo", false, "启用本地演示回复")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("无法识别的参数")
	}
	var cfg config
	cfg.Server.Listen = "127.0.0.1:8080"
	cfg.DataDir = "./data"
	cfg.Model.Provider = model.ProtocolChat
	cfg.Model.BaseURL = "https://api.openai.com/v1"
	cfg.Model.KeyRef = "env:CLUSBOT_MODEL_API_KEY"
	cfg.Runtime.MaxActiveRuns = 2
	cfg.Runtime.MaxRunSeconds = 600
	cfg.Runtime.ModelTimeoutSeconds = 120
	cfg.Runtime.MaxOutputTokens = 4096
	cfg.Runtime.MaxModelRequestBytes = model.DefaultMaxRequestBytes
	if *configPath != "" {
		f, err := os.Open(*configPath)
		if err != nil {
			return err
		}
		dec := yaml.NewDecoder(f)
		dec.KnownFields(true)
		err = dec.Decode(&cfg)
		f.Close()
		if err != nil {
			return err
		}
		if cfg.WorkspaceRoot != "" && !filepath.IsAbs(cfg.WorkspaceRoot) {
			cfg.WorkspaceRoot = filepath.Join(filepath.Dir(*configPath), cfg.WorkspaceRoot)
		}
		if !filepath.IsAbs(cfg.DataDir) {
			cfg.DataDir = filepath.Join(filepath.Dir(*configPath), cfg.DataDir)
		}
	}
	if *listen != "" {
		cfg.Server.Listen = *listen
	}
	if *dataDir != "" {
		cfg.DataDir = *dataDir
	}
	if *workspaceRoot != "" {
		cfg.WorkspaceRoot = *workspaceRoot
	}
	if cfg.WorkspaceRoot == "" {
		cfg.WorkspaceRoot = filepath.Join(cfg.DataDir, "workspace")
	}
	host, _, err := net.SplitHostPort(cfg.Server.Listen)
	if err != nil {
		return err
	}
	if host != "localhost" && (net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback()) {
		return errors.New("首期服务仅监听本机地址")
	}
	if cfg.Runtime.MaxActiveRuns < 1 || cfg.Runtime.MaxActiveRuns > 2 {
		return errors.New("max_active_runs 必须为 1 或 2")
	}
	if cfg.Runtime.MaxRunSeconds < 1 || cfg.Runtime.MaxRunSeconds > 86400 || cfg.Runtime.ModelTimeoutSeconds < 1 || cfg.Runtime.ModelTimeoutSeconds > 86400 {
		return errors.New("max_run_seconds 和 model_timeout_seconds 必须在 1 到 86400 之间")
	}
	if cfg.Runtime.MaxOutputTokens < 16 {
		return errors.New("max_output_tokens 必须至少为 16")
	}
	if cfg.Runtime.MaxModelRequestBytes < 4*1024 || cfg.Runtime.MaxModelRequestBytes > 2*1024*1024 {
		return errors.New("max_model_request_bytes 必须在 4096 到 2097152 之间")
	}
	if cfg.DataDir == "" {
		return errors.New("需要数据目录")
	}

	if cfg.Model.Provider != "" && cfg.Model.Provider != model.ProtocolResponses && cfg.Model.Provider != model.ProtocolChat {
		return errors.New("模型协议必须为 responses 或 chat_completions")
	}
	if err = os.MkdirAll(cfg.DataDir, 0700); err != nil {
		return err
	}
	lock, err := work.LockDataDir(cfg.DataDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	tokenPath := filepath.Join(cfg.DataDir, "access-token")
	token, err := accessToken(tokenPath)
	if err != nil {
		return err
	}
	db, err := work.Open(filepath.Join(cfg.DataDir, "clusbot.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	settings := &model.Settings{
		Store:           db,
		BaseURLOverride: strings.TrimSpace(os.Getenv("CLUSBOT_MODEL_BASE_URL")),
		ModelIDOverride: strings.TrimSpace(os.Getenv("CLUSBOT_MODEL_ID")),
		Demo:            *demo,
		MaxRequestBytes: cfg.Runtime.MaxModelRequestBytes,
	}
	initial, err := settings.Initialize(context.Background(), work.ModelSettings{
		Protocol: cfg.Model.Provider, BaseURL: cfg.Model.BaseURL, ModelID: cfg.Model.ID, KeyRef: cfg.Model.KeyRef,
	})
	if err != nil {
		return err
	}
	db.SetConnectionCredentialKey(token)
	if err = db.OpenUploads(filepath.Join(cfg.DataDir, "uploads")); err != nil {
		return err
	}
	workspacePath, err := filepath.Abs(cfg.WorkspaceRoot)
	if err != nil {
		return err
	}
	if err = db.OpenWorkspace(workspacePath); err != nil {
		return err
	}
	if err = db.RecoverPublications(); err != nil {
		return err
	}
	if err = db.Recover(); err != nil {
		return err
	}
	if err = db.ExpireApprovals(context.Background()); err != nil {
		return err
	}
	assets, err := fs.Sub(web.Assets, web.AssetDirectory)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	tcp := listener.Addr().(*net.TCPAddr)
	port := fmt.Sprint(tcp.Port)
	address := net.JoinHostPort(tcp.IP.String(), port)
	hosts := []string{address, net.JoinHostPort(host, port), net.JoinHostPort("localhost", port)}
	api := httpapi.Server{
		Store: db, Token: token, Hosts: hosts, Settings: settings, WorkspacePath: workspacePath,
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	server := &http.Server{Handler: api.Handler(assets), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	var serveErr error
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	workers := db.Start(ctx, settings, cfg.Runtime.MaxActiveRuns, work.RunLimits{
		ActiveTime:   time.Duration(cfg.Runtime.MaxRunSeconds) * time.Second,
		ModelTimeout: time.Duration(cfg.Runtime.ModelTimeoutSeconds) * time.Second,
		OutputTokens: cfg.Runtime.MaxOutputTokens,
	})
	approvalWorker := db.StartApprovalWorker(ctx)
	tokenPath, _ = filepath.Abs(tokenPath)
	slog.Info("ClusBot 已启动", "url", "http://"+address, "token_file", tokenPath, "model_mode", initial.Mode)
	if *printURL {
		fmt.Println("http://" + address)
	}
	select {
	case <-ctx.Done():
	case serveErr = <-done:
		cancel()
	}
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err = server.Shutdown(shutdown); err != nil {
		server.Close()
	}
	workers.Wait()
	approvalWorker.Wait()
	if errors.Is(serveErr, http.ErrServerClosed) {
		return nil
	}
	return serveErr
}

func accessToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		token := strings.TrimSpace(string(data))
		decoded, decodeErr := hex.DecodeString(token)
		if decodeErr != nil || len(decoded) != 32 {
			return "", errors.New("访问口令文件无效")
		}
		if err = os.Chmod(path, 0600); err != nil {
			return "", err
		}
		return token, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var bytes [32]byte
	rand.Read(bytes[:])
	token := hex.EncodeToString(bytes[:])
	if err = os.WriteFile(path, []byte(token+"\n"), 0600); err != nil {
		return "", err
	}
	return token, nil
}
