package work

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// HTTPBenchmarkProvider implements the narrow /start, /lookup, /get adapter
// contract. It never discovers endpoints supplied by model or job output.
type HTTPBenchmarkProvider struct {
	base, connection, credential, fingerprint string
	revision                                  int
	tools                                     map[string]bool
	client                                    *http.Client
}

func NewHTTPBenchmarkProvider(base, connection string, revision int, tools []string, token string) (*HTTPBenchmarkProvider, error) {
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" ||
		(u.Scheme != "https" && (u.Scheme != "http" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback())) {
		return nil, fmt.Errorf("%w：测试服务必须是固定 HTTPS 地址或本机回环 HTTP", ErrBenchmarkBinding)
	}
	if !validBenchKey(connection) || revision < 1 || len(tools) == 0 || token == "" || strings.ContainsAny(token, "\r\n") {
		return nil, ErrBenchmarkBinding
	}
	names := append([]string(nil), tools...)
	sort.Strings(names)
	allowed := map[string]bool{}
	for _, v := range names {
		if !validBenchKey(v) || allowed[v] {
			return nil, ErrBenchmarkBinding
		}
		allowed[v] = true
	}
	base = strings.TrimRight(u.String(), "/")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &HTTPBenchmarkProvider{base: base, connection: connection, revision: revision, tools: allowed, credential: token,
			fingerprint: digest(toJSON([]any{base, names, "benchmark-http-v1"})),
			client:      &http.Client{Transport: transport, Timeout: benchmarkCallTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}},
		nil
}

func (p *HTTPBenchmarkProvider) Supports(b BenchmarkBinding) bool {
	return p != nil && b.Connection == p.connection && b.Revision == p.revision && b.Validator == benchmarkValidatorV1 && p.tools[b.Tool]
}

func (p *HTTPBenchmarkProvider) Start(ctx context.Context, r BenchmarkJobRequest) (string, error) {
	var out struct {
		JobID string `json:"job_id"`
	}
	err := p.call(ctx, "start", r, "", &out)
	return out.JobID, err
}
func (p *HTTPBenchmarkProvider) Lookup(ctx context.Context, r BenchmarkJobRequest) (string, error) {
	var out struct {
		JobID string `json:"job_id"`
	}
	err := p.call(ctx, "lookup", r, "", &out)
	return out.JobID, err
}
func (p *HTTPBenchmarkProvider) Get(ctx context.Context, r BenchmarkJobRequest, job string) (BenchmarkJobResult, error) {
	var out BenchmarkJobResult
	err := p.call(ctx, "get", r, job, &out)
	return out, err
}

func (p *HTTPBenchmarkProvider) Cancel(ctx context.Context, r BenchmarkJobRequest, job string) error {
	var out struct {
		Acknowledged bool `json:"acknowledged"`
	}
	if err := p.call(ctx, "cancel", r, job, &out); err != nil {
		return err
	}
	if !out.Acknowledged {
		return ErrState
	}
	return nil
}

func (p *HTTPBenchmarkProvider) call(ctx context.Context, action string, in BenchmarkJobRequest, job string, out any) error {
	if !p.Supports(in.Binding) || in.IdempotencyKey == "" {
		return ErrBenchmarkBinding
	}
	payload, err := json.Marshal(struct {
		BenchmarkJobRequest
		JobID string `json:"job_id,omitempty"`
	}{in, job})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+"/"+action, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	// Do not advertise replayability to net/http. The provider receives a
	// stable key in the body, not an auto-retry-triggering header.
	req.GetBody = nil
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.credential)
	res, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("基准服务 %s 返回 %d", action, res.StatusCode)
	}
	const capBytes = 12 << 20
	body, err := io.ReadAll(io.LimitReader(res.Body, capBytes+1))
	if err != nil {
		return err
	}
	if len(body) > capBytes {
		return errors.New("基准服务响应超过上限")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err = dec.Decode(out); err != nil {
		return err
	}
	if !json.Valid(body) {
		return errors.New("基准服务响应不是单一 JSON")
	}
	return nil
}

// InstallBenchmarkProvider pins transport identity per connection revision.
// Rotating a credential keeps identity; changing host, path, or tools requires
// a new revision and never silently reroutes an in-flight job.
func (s *Store) InstallBenchmarkProvider(ctx context.Context, p *HTTPBenchmarkProvider) error {
	if p == nil {
		return ErrBenchmarkBinding
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO benchmark_provider_versions VALUES(?,?,?) ON CONFLICT(connection_key,revision) DO NOTHING`,
		p.connection, p.revision, p.fingerprint); err != nil {
		return err
	}
	var f string
	if err = tx.QueryRow(`SELECT fingerprint FROM benchmark_provider_versions WHERE connection_key=? AND revision=?`, p.connection, p.revision).Scan(&f); err != nil {
		return err
	}
	if f != p.fingerprint {
		return fmt.Errorf("%w：连接端点或工具改变，必须增加连接版本", ErrConflict)
	}
	return tx.Commit()
}
