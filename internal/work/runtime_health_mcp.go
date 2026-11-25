package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"clusbot/internal/mcpclient"
)

var ErrHealthCoverage = errors.New("健康巡检覆盖或结果不完整")

const (
	mcpNHC        = "nhc"
	mcpNHCUnknown = "unknown"
	mcpNHCHealthy = "healthy"
	mcpNHCFault   = "unhealthy"
)

// Only literal, already resolved targets can be checked: no host files,
// selectors, ranges, wildcard expansion or invocation options.
var healthNodeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,252}$`)
var mcpProviderTaskID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,255}$`)

type explicitMCPBinding struct {
	Binding CapabilityBinding
	Tool    mcpclient.Tool
}

// Do not fall back by tool name: a same-named tool on another provider (or a
// stale/revoked binding to a different revision) is a different authority.
func (s *Store) resolveExplicitMCPBinding(ctx context.Context, key string) (explicitMCPBinding, bool, error) {
	bindings, err := s.ListCapabilityBindings(ctx)
	if err != nil {
		return explicitMCPBinding{}, false, err
	}
	for _, b := range bindings {
		if b.CapabilityKey != key {
			continue
		}
		conn, err := s.Connection(ctx, b.ConnectionID)
		if err != nil || !conn.Enabled || conn.Status != "ready" || conn.Revision != b.ConnectionRevision {
			return explicitMCPBinding{}, true, fmt.Errorf("%w：%s 的连接未就绪或版本已变化，请重新绑定", ErrCapabilityBinding, key)
		}
		for _, t := range conn.Tools {
			if t.Name == b.StartTool && t.Allowed && t.SchemaError == "" {
				return explicitMCPBinding{Binding: b, Tool: t}, true, nil
			}
		}
		return explicitMCPBinding{}, true, fmt.Errorf("%w：%s 绑定的工具未授权或定义无效", ErrCapabilityBinding, key)
	}
	return explicitMCPBinding{}, false, nil
}

// Only an explicitly assigned NHC checker may produce NHC observations.
// In particular clusops fault_check is a Slurm scheduler snapshot; an empty
// listing means nothing about device health or benchmark coverage.
func (s *Store) callBoundNHC(ctx context.Context, capability string, nodes []string, includeStorage bool) (ConnectionResult, CapabilityBinding, bool, error) {
	b, found, err := s.resolveExplicitMCPBinding(ctx, capability)
	if err != nil || !found {
		return ConnectionResult{}, CapabilityBinding{}, found, err
	}
	t := b.Tool
	if b.Binding.Kind != "query" || t.Name != mcpNHC || !t.ReadOnly || t.ServerReadOnly == nil || !*t.ServerReadOnly || isToolDisallowedForReadOnly(t) {
		return ConnectionResult{}, b.Binding, true, fmt.Errorf("%w：%s 必须绑定已授权的只读 nhc；Slurm fault_check 不是设备健康检查", ErrCapabilityBinding, capability)
	}
	if len(nodes) == 0 || len(nodes) > 32 {
		return ConnectionResult{}, b.Binding, true, fmt.Errorf("%w：请明确指定 1 至 32 个节点", ErrHealthCoverage)
	}
	for _, node := range nodes {
		if !healthNodeName.MatchString(node) {
			return ConnectionResult{}, b.Binding, true, fmt.Errorf("%w：NHC 需要节点名称或 IPv4，不能用不透明资产编号和范围", ErrHealthCoverage)
		}
	}
	props := toolSchemaProperties(t)
	const cleanupMargin = 40 * time.Second
	executeSeconds := 300
	args := map[string]any{"nodes": strings.Join(nodes, ","), "sync": true}
	if includeStorage {
		executeSeconds = 480
		args["include_storage"] = true
	}
	args["execute_timeout"] = executeSeconds
	for field := range args {
		if _, ok := props[field]; !ok {
			return ConnectionResult{}, b.Binding, true, fmt.Errorf("%w：nhc 不提供必需的 %s 参数", ErrCapabilityBinding, field)
		}
	}
	res, err := s.callTrustedDomainConnectionTool(ctx, b.Binding.ConnectionID, t.Name, b.Binding.ConnectionRevision, args,
		time.Duration(executeSeconds)*time.Second+cleanupMargin)
	if err != nil {
		return ConnectionResult{}, b.Binding, true, err
	}
	current, exists, bindingErr := s.resolveExplicitMCPBinding(ctx, capability)
	if res.SourceUnavailable || bindingErr != nil || !exists || current.Binding != b.Binding || current.Tool.Version != t.Version {
		return ConnectionResult{}, b.Binding, true, fmt.Errorf("%w：巡检期间接入授权已变化或无法核实", ErrHealthCoverage)
	}
	if res.Truncated && res.Reference != nil {
		// The tool preview is capped. Restore only the actual archived response
		// with the exact still-current authority and hash, never parse text.
		raw, archiveErr := s.ConnectionEvidence(ctx, res.ID)
		var full mcpclient.Result
		if archiveErr != nil || res.Reference.ID != res.ID || len(raw) != res.Reference.Bytes ||
			digest(string(raw)) != res.Reference.SHA256 || json.Unmarshal(raw, &full) != nil {
			return ConnectionResult{}, b.Binding, true, fmt.Errorf("%w：无法核实完整巡检回执", ErrHealthCoverage)
		}
		res.Result = full
	}
	return res, b.Binding, true, nil
}

type mcpNHCReport struct {
	TaskID   string
	Status   string
	Nodes    map[string]string
	Complete bool
}

type mcpNHCNode struct {
	Node  string `json:"node"`
	State string `json:"state"`
	Code  *int   `json:"code"`
	Data  struct {
		CLISummary bool   `json:"cli_summary"`
		TaskID     string `json:"cli_task_id"`
		Source     string `json:"source"`
		Health     string `json:"health"`
		ExitCode   *int   `json:"exit_code"`
		Truncated  *bool  `json:"truncated"`
		Error      string `json:"error"`
	} `json:"data"`
}

// The exact Cluskit2 bridge contract is command_results[].data.health. The
// outer task's completion only describes the command. Positive evidence needs
// an unambiguous per-target result, plus complete, consistent coverage.
func parseMCPNHCResult(res ConnectionResult, expected []string) mcpNHCReport {
	report := mcpNHCReport{Nodes: make(map[string]string, len(expected))}
	for _, target := range expected {
		report.Nodes[target] = mcpNHCUnknown
	}
	if len(expected) == 0 || len(report.Nodes) != len(expected) || res.SourceUnavailable || res.Truncated || res.ValidationError != "" {
		return report
	}
	var payload struct {
		TaskID         string       `json:"task_id"`
		Mode           string       `json:"mode"`
		Terminal       *bool        `json:"terminal"`
		State          string       `json:"state"`
		Deduplicated   bool         `json:"deduplicated"`
		SummaryOmitted bool         `json:"summary_omitted"`
		ManifestError  string       `json:"manifest_error"`
		Partial        bool         `json:"partial_results"`
		Total          *int         `json:"total"`
		Succeeded      *int         `json:"succeeded"`
		Failed         *int         `json:"failed"`
		Canceled       *int         `json:"canceled"`
		Skipped        int          `json:"skipped"`
		Rows           []mcpNHCNode `json:"command_results"`
	}
	if json.Unmarshal(res.Structured, &payload) != nil || !mcpProviderTaskID.MatchString(payload.TaskID) ||
		payload.Mode != "sync" || payload.Terminal == nil || !*payload.Terminal ||
		(payload.State != "COMPLETE" && payload.State != "FAILED") ||
		payload.Deduplicated || payload.SummaryOmitted || payload.ManifestError != "" {
		return report
	}
	report.TaskID, report.Status = payload.TaskID, payload.State
	if payload.Total == nil || *payload.Total != len(expected) || payload.Succeeded == nil || payload.Failed == nil ||
		payload.Canceled == nil || *payload.Succeeded < 0 || *payload.Failed < 0 ||
		*payload.Canceled != 0 || payload.Skipped != 0 || *payload.Succeeded+*payload.Failed != *payload.Total ||
		payload.Partial || (payload.State == "COMPLETE" && (res.IsError || *payload.Failed != 0)) {
		return report
	}
	seen := map[string]bool{}
	var successRows, failureRows int
	for _, item := range payload.Rows {
		switch {
		case item.Data.CLISummary && item.Node == "":
			continue // synthetic CLI summary row, never an asset receipt
		case item.State == "succeeded":
			successRows++
		case item.State == "failed":
			failureRows++
		default:
			return unknownNHCReport(report)
		}
		if _, wanted := report.Nodes[item.Node]; !wanted || seen[item.Node] || item.Data.CLISummary {
			return unknownNHCReport(report)
		}
		seen[item.Node] = true
		d := item.Data
		if d.TaskID != report.TaskID || d.Source != mcpNHC || item.Code == nil || d.ExitCode == nil ||
			d.Truncated == nil || *d.Truncated || d.Error != "" {
			continue
		}
		switch {
		case d.Health == mcpNHCHealthy && item.State == "succeeded" && *item.Code == 0 && *d.ExitCode == 0:
			report.Nodes[item.Node] = mcpNHCHealthy
		case d.Health == mcpNHCFault && item.State == "failed" && *item.Code == 1 &&
			*d.ExitCode >= 0 && *d.ExitCode != 126 && *d.ExitCode != 127 && *d.ExitCode != 255:
			// A real NHC finding makes the overall tool an MCP error. It still
			// documents the explicit fault, not successful verification.
			report.Nodes[item.Node] = mcpNHCFault
		}
	}
	if successRows > *payload.Succeeded || failureRows > *payload.Failed {
		return unknownNHCReport(report)
	}
	if len(seen) != len(expected) || successRows != *payload.Succeeded || failureRows != *payload.Failed {
		// Keep attributable negatives from partial coverage, never a positive.
		for ref, state := range report.Nodes {
			if state == mcpNHCHealthy {
				report.Nodes[ref] = mcpNHCUnknown
			}
		}
		return report
	}
	report.Complete = true
	for ref, state := range report.Nodes {
		if state == mcpNHCHealthy && (res.IsError || payload.State != "COMPLETE") {
			// Positive nodes in a batch with a non-success envelope are not
			// independent proof; only explicit unhealthy nodes may be stored.
			report.Nodes[ref] = mcpNHCUnknown
			state = mcpNHCUnknown
		}
		report.Complete = report.Complete && state != mcpNHCUnknown
	}
	return report
}

func unknownNHCReport(r mcpNHCReport) mcpNHCReport {
	for ref := range r.Nodes {
		r.Nodes[ref] = mcpNHCUnknown
	}
	return r
}

func mcpNHCEvidence(res ConnectionResult, report mcpNHCReport, objectRef, state string) string {
	return fmt.Sprintf("NHC %s：节点 %s；任务 %s；接入回执 %s", state, objectRef, report.TaskID, res.ID)
}
