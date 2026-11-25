package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"clusbot/internal/mcpclient"
)

type benchmarkBindingReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type benchmarkProviderSelection struct {
	provider          BenchmarkProvider
	pin               string
	connectionID      string
	capabilityKey     string
	capabilityVersion int
	canStop           bool
}

func (p benchmarkProviderSelection) requestDigest(req BenchmarkJobRequest) string {
	if p.pin == "" {
		return digest(toJSON(req))
	}
	return digest(toJSON([]any{req, p.pin}))
}

func selectBenchmarkProvider(ctx context.Context, db benchmarkBindingReader, provider BenchmarkProvider, binding BenchmarkBinding) (benchmarkProviderSelection, error) {
	if err := ctx.Err(); err != nil {
		return benchmarkProviderSelection{}, err
	}
	if dynamic, ok := provider.(*dynamicBenchmarkProvider); ok {
		return dynamic.resolve(ctx, db, binding)
	}
	if provider == nil || !provider.Supports(binding) {
		return benchmarkProviderSelection{}, ErrBenchmarkBinding
	}
	_, canStop := provider.(benchmarkCanceller)
	selection := benchmarkProviderSelection{provider: provider, canStop: canStop}
	switch p := provider.(type) {
	case *MCPBenchmarkProvider:
		selection.canStop = p.binding.CancelTool != ""
		selection.pin = p.pin
		if selection.pin == "" {
			selection.pin = digest(toJSON([]any{"mcp", p.connection, p.revision, p.binding}))
		}
	case *HTTPBenchmarkProvider:
		selection.pin = digest(toJSON([]any{"http", p.connection, p.revision, p.fingerprint}))
	}
	return selection, nil
}

func (p benchmarkProviderSelection) validate(req BenchmarkJobRequest) error {
	if mcp, ok := p.provider.(*MCPBenchmarkProvider); ok && mcp.binding.StartTool == "bench_run" {
		args, err := cluskitBenchmarkArguments(req)
		if err != nil {
			return err
		}
		if mcp.startDefinition != nil {
			if err := mcpclient.ValidateArguments(*mcp.startDefinition, args); err != nil {
				return fmt.Errorf("%w：基准参数不符合固定的 MCP 工具定义: %v", ErrBenchmarkInput, err)
			}
		}
	}
	return nil
}

func benchmarkCaseRequest(c BenchmarkCase, key string) BenchmarkJobRequest {
	return BenchmarkJobRequest{Binding: c.Binding, Case: c.Key, Targets: c.Targets, Parameters: c.Parameters, IdempotencyKey: key}
}

// Resolve through the caller's transaction if one is already open. Requesting
// another connection from the single-writer pool would deadlock, even if no
// binding matches and the real transport is HTTP.
func (p *dynamicBenchmarkProvider) resolve(ctx context.Context, db benchmarkBindingReader, b BenchmarkBinding) (benchmarkProviderSelection, error) {
	if p == nil || p.store == nil || db == nil || b.Validator != benchmarkValidatorV1 {
		return benchmarkProviderSelection{}, ErrBenchmarkBinding
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	rows, err := db.QueryContext(ctx, `SELECT c.capability_id,c.capability_key,c.version,p.provider_id,cc.connection_id,p.config_ref,
 mc.id,mc.protocol,mc.credential_binding
FROM capabilities c
JOIN capability_providers p ON p.kind='mcp' AND p.capabilities=json_array(c.capability_key||':'||c.version)
JOIN capability_connections cc ON cc.provider_id=p.provider_id AND cc.enabled=1
JOIN connections mc ON mc.id=cc.credential_ref AND mc.revision=cc.connection_version AND mc.enabled=1 AND mc.status='ready'
WHERE c.kind='action' AND c.execution_mode='async'
AND cc.connection_version=? AND (mc.id=? OR mc.name=?)
AND json_extract(p.config_ref,'$.start_tool')=? LIMIT 2`, b.Revision, b.Connection, b.Connection, b.Tool)
	if err != nil {
		return benchmarkProviderSelection{}, err
	}
	var capID, capKey, providerID, linkID, raw, connID, protocol, credentialBinding string
	var capVersion int
	found := rows.Next()
	if found {
		err = rows.Scan(&capID, &capKey, &capVersion, &providerID, &linkID, &raw, &connID, &protocol, &credentialBinding)
		if err == nil && rows.Next() {
			err = fmt.Errorf("%w：多个基准能力匹配同一工具", ErrBenchmarkBinding)
		}
	}
	if err == nil {
		err = rows.Err()
	}
	closeErr := rows.Close()
	if err != nil {
		return benchmarkProviderSelection{}, err
	}
	if closeErr != nil {
		return benchmarkProviderSelection{}, closeErr
	}
	if !found {
		return selectBenchmarkProvider(ctx, db, p.fallback, b)
	}
	var binding struct {
		ConnectionID string `json:"connection_id"`
		MCPToolBinding
	}
	if err := json.Unmarshal([]byte(raw), &binding); err != nil || binding.ConnectionID != connID || binding.LookupTool == "" || binding.GetTool == "" {
		return benchmarkProviderSelection{}, ErrBenchmarkBinding
	}
	versions := map[string]string{}
	var start mcpclient.Tool
	for _, name := range []string{binding.StartTool, binding.LookupTool, binding.GetTool, binding.CancelTool} {
		if name == "" || versions[name] != "" {
			continue
		}
		var rawTool string
		var approvedReadOnly bool
		if err := db.QueryRowContext(ctx, `SELECT definition,read_only FROM connection_tools WHERE connection_id=? AND name=? AND allowed=1`, connID, name).Scan(&rawTool, &approvedReadOnly); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return benchmarkProviderSelection{}, ErrBenchmarkBinding
			}
			return benchmarkProviderSelection{}, err
		}
		var tool mcpclient.Tool
		if err := json.Unmarshal([]byte(rawTool), &tool); err != nil || tool.Name != name || tool.Version == "" {
			return benchmarkProviderSelection{}, ErrBenchmarkBinding
		}
		if name == binding.LookupTool || name == binding.GetTool {
			if !approvedReadOnly || tool.ServerReadOnly == nil || !*tool.ServerReadOnly {
				return benchmarkProviderSelection{}, fmt.Errorf("%w：对账工具 %s 不是已授权的只读工具", ErrBenchmarkBinding, name)
			}
		}
		versions[name] = tool.Version
		if name == binding.StartTool {
			start = tool
		}
	}
	actual, err := NewMCPBenchmarkProvider(b.Connection, b.Revision, binding.MCPToolBinding, func(ctx context.Context, name string, args map[string]any) (mcpclient.Result, error) {
		res, err := p.store.CallConnectionTool(ctx, connID, name, b.Revision, args)
		if err != nil {
			return res.Result, err
		}
		if res.SourceUnavailable {
			return mcpclient.Result{}, ErrBenchmarkBinding
		}
		if res.Result.Truncated && res.Reference != nil {
			archived, err := p.store.ConnectionEvidence(ctx, res.ID)
			if err != nil {
				return mcpclient.Result{}, err
			}
			var full mcpclient.Result
			if err := json.Unmarshal(archived, &full); err != nil {
				return mcpclient.Result{}, err
			}
			return full, nil
		}
		return res.Result, nil
	})
	if err != nil {
		return benchmarkProviderSelection{}, err
	}
	actual.pin = digest(toJSON([]any{"mcp", capID, providerID, linkID, connID, b.Revision, protocol, credentialBinding, binding.MCPToolBinding, versions}))
	actual.startDefinition = &start
	return benchmarkProviderSelection{provider: actual, pin: actual.pin, connectionID: connID, capabilityKey: capKey, capabilityVersion: capVersion, canStop: actual.binding.CancelTool != ""}, nil
}
