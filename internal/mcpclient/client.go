package mcpclient

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var (
	ErrConfig        = errors.New("请输入有效的连接配置")
	ErrCredentials   = errors.New("启动环境缺少所需凭据或变量")
	ErrConnect       = errors.New("无法建立 MCP 会话，请检查配置和服务状态")
	ErrDiscovery     = errors.New("MCP 工具发现失败或超过发现上限")
	ErrSchemaChanged = errors.New("连接凭据、协议或工具定义已变更，请重新测试连接并授权")
	ErrArguments     = errors.New("参数必须是符合工具 Schema 的 JSON 对象")
	ErrCall          = errors.New("未收到完整的 MCP 工具结果，请检查服务状态")
	ErrToolInput     = errors.New("服务要求额外输入，当前连接未执行后续步骤")
)

const (
	Timeout     = 12 * time.Second
	maxResponse = 1024 * 1024
	MaxResult   = 16 * 1024
)

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,79}$`)

type Config struct {
	Transport string   `json:"transport"`
	Endpoint  string   `json:"endpoint,omitempty"`
	TokenEnv  string   `json:"token_env,omitempty"`
	Command   string   `json:"command,omitempty"`
	Args      []string `json:"args,omitempty"`
	Env       []string `json:"env,omitempty"`
}

func (cfg Config) Validate() error {
	switch cfg.Transport {
	case "stdio":
		if !filepath.IsAbs(cfg.Command) || len(cfg.Command) > 1024 || strings.ContainsAny(cfg.Command, "\r\n\x00") ||
			cfg.Endpoint != "" || cfg.TokenEnv != "" || len(cfg.Args) > 24 || len(cfg.Env) > 24 {
			return ErrConfig
		}
		for _, arg := range cfg.Args {
			if len(arg) > 1024 || strings.ContainsRune(arg, 0) {
				return ErrConfig
			}
		}
		for _, name := range cfg.Env {
			if !envName.MatchString(name) {
				return ErrConfig
			}
		}
	case "http":
		if cfg.Command != "" || len(cfg.Args) > 0 || len(cfg.Env) > 0 ||
			(cfg.TokenEnv != "" && !envName.MatchString(cfg.TokenEnv)) {
			return ErrConfig
		}
		_, err := endpointURL(cfg.Endpoint)
		return err
	default:
		return ErrConfig
	}
	return nil
}

func endpointURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.ForceQuery ||
		(u.Scheme != "https" && (u.Scheme != "http" || !net.ParseIP(u.Hostname()).IsLoopback())) {
		return nil, fmt.Errorf("%w。服务地址须为 HTTPS，或指向本机回环 IP 的 HTTP 地址", ErrConfig)
	}
	return u, nil
}

func (cfg Config) credentialNames() []string {
	switch {
	case cfg.Transport == "stdio":
		return cfg.Env
	case cfg.TokenEnv != "":
		return []string{cfg.TokenEnv}
	}
	return nil
}

func (cfg Config) CredentialBinding(key []byte) (string, error) {
	names := cfg.credentialNames()
	if len(names) == 0 {
		return "", nil
	}
	values := make(map[string]string, len(names))
	for _, name := range names {
		value, ok := os.LookupEnv(name)
		if !ok {
			return "", ErrCredentials
		}
		values[name] = value
	}
	payload, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	hash := hmac.New(sha256.New, key)
	hash.Write(payload)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type Tool struct {
	Name         string          `json:"name"`
	Title        string          `json:"title,omitempty"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema"`
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
	SchemaError  string          `json:"schema_error,omitempty"`
	Version      string          `json:"version"`
	Allowed      bool            `json:"allowed"`
}

type Catalog struct {
	Protocol string `json:"protocol,omitempty"`
	Tools    []Tool `json:"tools"`
}

type Result struct {
	Text            string           `json:"text,omitempty"`
	Structured      json.RawMessage  `json:"structured,omitempty"`
	IsError         bool             `json:"is_error"`
	Truncated       bool             `json:"truncated"`
	MediaOmitted    bool             `json:"media_omitted"`
	Reference       *ResultReference `json:"reference,omitempty"`
	ValidationError string           `json:"validation_error,omitempty"`
}

type ResultReference struct {
	ID     string `json:"id"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

func Connect(ctx context.Context, cfg Config) (*mcp.ClientSession, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	var transport mcp.Transport
	if cfg.Transport == "stdio" {
		cmd := exec.CommandContext(ctx, cfg.Command, cfg.Args...)
		cmd.Env = []string{}
		cmd.Stderr = io.Discard
		cmd.WaitDelay = 500 * time.Millisecond
		for _, name := range cfg.Env {
			value, ok := os.LookupEnv(name)
			if !ok {
				return nil, ErrCredentials
			}
			cmd.Env = append(cmd.Env, name+"="+value)
		}
		transport = &mcp.CommandTransport{Command: cmd, TerminateDuration: 250 * time.Millisecond}
	} else {
		endpoint, _ := endpointURL(cfg.Endpoint)
		var token string
		if cfg.TokenEnv != "" {
			token = os.Getenv(cfg.TokenEnv)
			if token == "" || strings.ContainsAny(token, "\r\n") {
				return nil, ErrCredentials
			}
		}
		rt := http.DefaultTransport.(*http.Transport).Clone()
		rt.Proxy = nil
		rt.DisableKeepAlives = true
		transport = &mcp.StreamableClientTransport{
			Endpoint: cfg.Endpoint, MaxRetries: -1, DisableStandaloneSSE: true, MaxEventSize: maxResponse,
			HTTPClient: &http.Client{Transport: &boundTransport{base: rt, endpoint: endpoint, token: token}, Timeout: Timeout,
				CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "ClusBot", Version: "0.3"}, &mcp.ClientOptions{
		Capabilities: &mcp.ClientCapabilities{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, ErrConnect
	}
	return session, nil
}

func Discover(ctx context.Context, session *mcp.ClientSession) (Catalog, error) {
	catalog := Catalog{Tools: []Tool{}}
	if init := session.InitializeResult(); init != nil {
		catalog.Protocol = init.ProtocolVersion
		if init.Capabilities == nil || init.Capabilities.Tools == nil {
			return catalog, nil
		}
	}
	var cursor string
	seen := map[string]bool{}
	for range 8 {
		page, err := session.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return Catalog{}, ErrDiscovery
		}
		for _, t := range page.Tools {
			if t == nil || t.Name == "" || len(t.Name) > 200 || seen[t.Name] || len(catalog.Tools) == 100 {
				return Catalog{}, ErrDiscovery
			}
			schema, err := json.Marshal(t.InputSchema)
			if err != nil || len(schema) > 24*1024 {
				return Catalog{}, ErrDiscovery
			}
			identity, err := json.Marshal(t)
			if err != nil || len(identity) > 48*1024 {
				return Catalog{}, ErrDiscovery
			}
			hash := sha256.Sum256(identity)
			seen[t.Name] = true
			tool := Tool{Name: t.Name, Title: t.Title, Description: t.Description, InputSchema: schema, Version: hex.EncodeToString(hash[:])}
			if t.OutputSchema != nil {
				tool.OutputSchema, err = json.Marshal(t.OutputSchema)
				if err != nil || len(tool.OutputSchema) > 24*1024 {
					return Catalog{}, ErrDiscovery
				}
			}
			if err = ValidateToolSchemas(tool); err != nil {
				tool.SchemaError = err.Error()
			}
			catalog.Tools = append(catalog.Tools, tool)
		}
		if page.NextCursor == "" {
			return catalog, nil
		}
		if page.NextCursor == cursor {
			break
		}
		cursor = page.NextCursor
	}
	return Catalog{}, ErrDiscovery
}

func ValidateArguments(tool Tool, args map[string]any) error {
	if args == nil {
		return ErrArguments
	}
	if err := ValidateToolSchemas(tool); err != nil {
		return err
	}
	resolved, err := compileSchema(tool.InputSchema)
	if err != nil || resolved.Validate(args) != nil {
		return ErrArguments
	}
	return nil
}

func Call(ctx context.Context, session *mcp.ClientSession, tool Tool, args map[string]any, cfg Config) (Result, []byte, error) {
	value, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool.Name, Arguments: args})
	if err != nil || value == nil {
		return Result{}, nil, ErrCall
	}
	if value.NeedsInput() {
		return Result{}, nil, ErrToolInput
	}
	redactor := resultRedactor(cfg)
	full := Result{IsError: value.IsError}
	if !value.IsError {
		full.ValidationError = validateOutput(tool, value.StructuredContent)
		full.IsError = full.ValidationError != ""
	}
	var text strings.Builder
	for _, content := range value.Content {
		entry, ok := content.(*mcp.TextContent)
		if !ok {
			full.MediaOmitted = true
			continue
		}
		if text.Len() > 0 && text.Len() < maxResponse {
			text.WriteByte('\n')
		}
		part := entry.Text
		if redactor != nil {
			part = redactor.Replace(part)
		}
		bounded := textPrefix(part, maxResponse-text.Len())
		full.Truncated = full.Truncated || len(bounded) < len(part)
		text.WriteString(bounded)
	}
	full.Text = text.String()
	if value.StructuredContent != nil {
		structured := value.StructuredContent
		if redactor != nil {
			structured = redactStructured(structured, redactor)
		}
		encoded, err := json.Marshal(structured)
		if err != nil || len(encoded) > maxResponse {
			full.Truncated = true
		} else {
			full.Structured = encoded
		}
	}
	preview := full
	preview.Text = textPrefix(full.Text, MaxResult)
	if len(preview.Structured) > MaxResult-len(preview.Text) {
		preview.Structured = nil
	}
	preview.Truncated = full.Truncated || len(preview.Text) < len(full.Text) || len(preview.Structured) < len(full.Structured)
	if (!preview.Truncated && preview.ValidationError == "") || full.Truncated {
		return preview, nil, nil
	}
	archive, err := json.Marshal(full)
	if err != nil || len(archive) > maxResponse {
		return preview, nil, nil
	}
	return preview, archive, nil
}

func textPrefix(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	for !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit]
}

func resultRedactor(cfg Config) *strings.Replacer {
	var values []string
	for _, name := range cfg.credentialNames() {
		if value := os.Getenv(name); value != "" {
			values = append(values, value)
		}
	}
	if len(values) == 0 {
		return nil
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	pairs := make([]string, 0, 2*len(values))
	for _, value := range values {
		pairs = append(pairs, value, "[已隐藏]")
	}
	return strings.NewReplacer(pairs...)
}

func redactStructured(value any, redactor *strings.Replacer) any {
	switch item := value.(type) {
	case string:
		return redactor.Replace(item)
	case map[string]any:
		result := make(map[string]any, len(item))
		for key, member := range item {
			result[redactor.Replace(key)] = redactStructured(member, redactor)
		}
		return result
	case []any:
		for i, member := range item {
			item[i] = redactStructured(member, redactor)
		}
	}
	return value
}

type boundTransport struct {
	base     http.RoundTripper
	endpoint *url.URL
	token    string
}

func (t *boundTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	u := request.URL
	if u.Scheme != t.endpoint.Scheme || u.Host != t.endpoint.Host || u.EscapedPath() != t.endpoint.EscapedPath() || u.RawQuery != "" {
		return nil, ErrConnect
	}
	if t.token != "" {
		request = request.Clone(request.Context())
		request.Header.Set("Authorization", "Bearer "+t.token)
	}
	response, err := t.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	response.Body = &boundedBody{ReadCloser: response.Body, left: maxResponse}
	return response, nil
}

type boundedBody struct {
	io.ReadCloser
	left int
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if len(p) > b.left+1 {
		p = p[:b.left+1]
	}
	n, err := b.ReadCloser.Read(p)
	if n > b.left {
		return 0, ErrDiscovery
	}
	b.left -= n
	return n, err
}
