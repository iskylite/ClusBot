package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"clusbot/internal/httpapi"
	"clusbot/internal/model"
	"clusbot/internal/work"

	"gopkg.in/yaml.v3"
)

type config struct {
	Server struct {
		Listen string `yaml:"listen"`
		Mode   string `yaml:"mode"`
		Origin string `yaml:"origin"`
	} `yaml:"server"`
	DataDir string `yaml:"data_dir"`
	Tickets struct {
		BaseURL             string `yaml:"base_url"`
		HealthURL           string `yaml:"health_url"`
		CredentialEnv       string `yaml:"credential_env"`
		HealthCredentialEnv string `yaml:"health_credential_env"`
	} `yaml:"tickets"`
	Benchmark struct {
		BaseURL        string   `yaml:"base_url"`
		Connection     string   `yaml:"connection"`
		Revision       int      `yaml:"revision"`
		Tools          []string `yaml:"tools"`
		CredentialEnv  string   `yaml:"credential_env"`
		EventSecretEnv string   `yaml:"event_secret_env"`
		EnableDispatch bool     `yaml:"enable_dispatch"`
		CluskitStdio   struct {
			Command     string            `yaml:"command"`
			Environment []string          `yaml:"environment"`
			ToolDigests map[string]string `yaml:"tool_digests"`
		} `yaml:"cluskit_stdio"`
	} `yaml:"benchmark"`
	Discovery struct {
		BaseURL       string   `yaml:"base_url"`
		Source        string   `yaml:"source"`
		Scopes        []string `yaml:"scopes"`
		CredentialEnv string   `yaml:"credential_env"`
	} `yaml:"discovery"`
	Model struct {
		Provider string `yaml:"provider"`
		BaseURL  string `yaml:"base_url"`
		ID       string `yaml:"model_id"`
		KeyRef   string `yaml:"api_key_ref"`
	} `yaml:"model"`
	Runtime struct {
		MaxActiveRuns            int  `yaml:"max_active_runs"`
		MaxPendingExplicitTasks  int  `yaml:"max_pending_explicit_tasks"`
		MaxRunSeconds            int  `yaml:"max_run_seconds"`
		ModelTimeoutSeconds      int  `yaml:"model_timeout_seconds"`
		MaxOutputTokens          int  `yaml:"max_output_tokens"`
		MaxModelRequestBytes     int  `yaml:"max_model_request_bytes"`
		ModelRunPriorityFairness bool `yaml:"model_run_priority_fairness"`
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
		case "import-domain-assets":
			return runImportDomainAssets(args[1:])
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
	mode := flags.String("mode", "", "监听方式：loopback（默认，本机浏览器或 App）、server（HTTPS 反代）")
	origin := flags.String("origin", "", "服务器模式对外 HTTPS Origin")
	dataDir := flags.String("data-dir", "", "覆盖数据目录")
	printURL := flags.Bool("print-url", false, "服务就绪后向标准输出写入本机 URL")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("无法识别的参数")
	}
	var cfg config
	cfg.Server.Listen = "127.0.0.1:8080"
	cfg.Server.Mode = "loopback"
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
		if cfg.DataDir != "" && !filepath.IsAbs(cfg.DataDir) {
			cfg.DataDir = filepath.Join(filepath.Dir(*configPath), cfg.DataDir)
		}
	}
	if *listen != "" {
		cfg.Server.Listen = *listen
	}
	if *mode != "" {
		cfg.Server.Mode = *mode
	}
	if *origin != "" {
		cfg.Server.Origin = *origin
	}
	switch cfg.Server.Mode {
	case "server":
		origin, err := publicHTTPSOrigin(cfg.Server.Origin)
		if err != nil {
			return err
		}
		cfg.Server.Origin = origin
	case "loopback":
		if cfg.Server.Origin != "" {
			return errors.New("只有 server 监听方式接受 server.origin")
		}
	default:
		return errors.New("server.mode 必须为 loopback 或 server；不再支持旧的共享口令启动")
	}
	if *dataDir != "" {
		cfg.DataDir = *dataDir
	}
	host, _, err := net.SplitHostPort(cfg.Server.Listen)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return errors.New("仅支持回环 IP 监听；对外服务请用 HTTPS 反向代理")
	}

	if cfg.Runtime.MaxActiveRuns < 1 || cfg.Runtime.MaxActiveRuns > 2 {
		return errors.New("max_active_runs 必须为 1 或 2")
	}
	if cfg.Runtime.MaxPendingExplicitTasks < 0 || cfg.Runtime.MaxPendingExplicitTasks > work.MaxExplicitTaskAdmissionLimit {
		return fmt.Errorf("max_pending_explicit_tasks 必须在 0 到 %d 之间，0 表示不限制显式新任务受理", work.MaxExplicitTaskAdmissionLimit)
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
	if cfg.Tickets.BaseURL != "" || cfg.Tickets.HealthURL != "" || cfg.Tickets.CredentialEnv != "" || cfg.Tickets.HealthCredentialEnv != "" {
		return errors.New("账号服务尚不支持旧版 tickets.* 实例级工单配置；请保留该配置用于后续按账号绑定，不会启动旧的共享工单入口")
	}
	if err := validateAccountBenchmarkConfig(cfg); err != nil {
		return err
	}
	if err = os.MkdirAll(cfg.DataDir, 0700); err != nil {
		return err
	}
	cfg.DataDir, err = filepath.EvalSymlinks(cfg.DataDir)
	if err != nil {
		return err
	}
	lock, err := work.LockDataDir(cfg.DataDir)
	if err != nil {
		return err
	}
	defer lock.Close()

	ctx := context.Background()
	db, err := openAccountInstance(ctx, cfg.DataDir)
	if err != nil {
		return err
	}
	defer db.Close()
	configure := func(ctx context.Context, api *httpapi.Server) (func(), error) {
		return startAccountRuntime(ctx, api, cfg)
	}
	if cfg.Server.Mode == "server" {
		return serveAccounts(db, cfg.Server.Listen, cfg.Server.Origin, *printURL, configure)
	}
	return serveLoopbackAccounts(db, cfg.Server.Listen, *printURL, configure)
}

func publicHTTPSOrigin(raw string) (string, error) {
	invalid := errors.New("server.origin 必须为对外 HTTPS Origin，例如 https://example.com")
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" ||
		(u.EscapedPath() != "" && u.EscapedPath() != "/") || u.RawQuery != "" ||
		u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") || strings.HasSuffix(u.Host, ":") {
		return "", invalid
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") && (!strings.HasPrefix(u.Host, "[") || net.ParseIP(host) == nil) {
		return "", invalid
	}
	port := 443
	if u.Port() != "" {
		port, err = strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return "", invalid
		}
	}
	if port != 443 {
		host = net.JoinHostPort(host, strconv.Itoa(port))
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return "https://" + host, nil
}
