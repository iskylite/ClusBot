package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type FaultSegmentInput struct {
	FaultKey    string `json:"fault_key"`
	ObjectRef   string `json:"object_ref"`
	FieldKey    string `json:"field_key,omitempty"`
	Value       string `json:"value,omitempty"`
	SourceRef   string `json:"source_ref,omitempty"`
	WindowStart int64  `json:"window_start,omitempty"`
	WindowEnd   int64  `json:"window_end,omitempty"`
	State       string `json:"state,omitempty"`
}

type FaultSegmentSummary struct {
	SegmentID     string `json:"segment_id"`
	ObservationID string `json:"observation_id"`
	FaultKey      string `json:"fault_key"`
	ObjectRef     string `json:"object_ref"`
	State         string `json:"state"`
	WindowStart   int64  `json:"window_start"`
	WindowEnd     int64  `json:"window_end"`
}

// RecordFaultSegment persists an observation row and a linked fault_segments row
// so ClusterTrendStats can attribute faults to concrete asset object_refs.
func (s *Store) RecordFaultSegment(ctx context.Context, in FaultSegmentInput) (FaultSegmentSummary, error) {
	in.FaultKey = strings.TrimSpace(in.FaultKey)
	in.ObjectRef = strings.TrimSpace(in.ObjectRef)
	in.FieldKey = strings.TrimSpace(in.FieldKey)
	in.Value = strings.TrimSpace(in.Value)
	in.SourceRef = strings.TrimSpace(in.SourceRef)
	in.State = strings.TrimSpace(in.State)
	if in.FaultKey == "" || in.ObjectRef == "" {
		return FaultSegmentSummary{}, errors.New("fault_key 和 object_ref 不能为空")
	}
	if in.FieldKey == "" {
		in.FieldKey = "fault_status"
	}
	if in.Value == "" {
		in.Value = "unhealthy"
	}
	if in.SourceRef == "" {
		in.SourceRef = "manual"
	}
	if in.State == "" {
		in.State = "active"
	}
	switch in.State {
	case "active", "recovered", "recurred":
	default:
		return FaultSegmentSummary{}, fmt.Errorf("无效的故障段状态：%s", in.State)
	}
	stamp := now()
	if in.WindowStart == 0 {
		in.WindowStart = stamp
	}
	if in.WindowEnd == 0 {
		in.WindowEnd = in.WindowStart
	}
	if in.WindowStart > in.WindowEnd {
		return FaultSegmentSummary{}, errors.New("window_start 不能大于 window_end")
	}

	obsID := id()
	segID := id()
	refsJSON := toJSON([]string{obsID})

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return FaultSegmentSummary{}, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `INSERT INTO observations(observation_id,source_ref,source_identity,collected_at,coverage,object_ref,field_key,value,unit,recorded_at)
VALUES(?,?,?,?,?,?,?,?,?,?)`,
		obsID, in.SourceRef, in.SourceRef, in.WindowStart, "fault_segment", in.ObjectRef, in.FieldKey, in.Value, "", stamp); err != nil {
		return FaultSegmentSummary{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO fault_segments(segment_id,fault_key,observation_refs,window_start,window_end,state,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?)`,
		segID, in.FaultKey, refsJSON, in.WindowStart, in.WindowEnd, in.State, stamp, stamp); err != nil {
		return FaultSegmentSummary{}, err
	}
	if err := tx.Commit(); err != nil {
		return FaultSegmentSummary{}, err
	}
	return FaultSegmentSummary{
		SegmentID:     segID,
		ObservationID: obsID,
		FaultKey:      in.FaultKey,
		ObjectRef:     in.ObjectRef,
		State:         in.State,
		WindowStart:   in.WindowStart,
		WindowEnd:     in.WindowEnd,
	}, nil
}

// SyncFaultSegmentsFromMCP calls a bound/allowed `fault_check` MCP tool and persists
// any unhealthy findings as observations + fault_segments.
func (s *Store) SyncFaultSegmentsFromMCP(ctx context.Context, scope string) ([]FaultSegmentSummary, error) {
	extra := map[string]any{"sync": true}
	res, binding, ok, err := s.CallBoundMCPTargetTool(ctx, "ops.fault_check", "fault_check", strings.TrimSpace(scope), extra)
	if !ok {
		res, binding, ok, err = s.CallBoundMCPTargetTool(ctx, "", "fault_check", strings.TrimSpace(scope), extra)
	}
	if !ok {
		return nil, fmt.Errorf("%w：未配置或未启用 fault_check MCP 工具", ErrCapabilityBinding)
	}
	if err != nil {
		return nil, err
	}

	sourceRef := "mcp:" + firstNonEmpty(binding.ConnectionName, binding.ConnectionID, "fault_check")
	inputs := parseMCPFaultFindings(res, scope, sourceRef)
	out := make([]FaultSegmentSummary, 0, len(inputs))
	for _, in := range inputs {
		item, recErr := s.RecordFaultSegment(ctx, in)
		if recErr != nil {
			return out, recErr
		}
		out = append(out, item)
	}
	return out, nil
}

func parseMCPFaultFindings(res ConnectionResult, scope, sourceRef string) []FaultSegmentInput {
	raw := res.Structured
	if len(raw) == 0 {
		raw = []byte(strings.TrimSpace(res.Text))
	}
	type finding struct {
		Host     string `json:"host"`
		Node     string `json:"node"`
		Object   string `json:"object_ref"`
		FaultKey string `json:"fault_key"`
		Check    string `json:"check"`
		Status   string `json:"status"`
		Healthy  *bool  `json:"healthy"`
		Detail   string `json:"detail"`
	}
	var env struct {
		Findings []finding `json:"findings"`
		Failures []finding `json:"failures"`
		Results  []finding `json:"results"`
	}
	var out []FaultSegmentInput
	if len(raw) > 0 && json.Unmarshal(raw, &env) == nil {
		items := append(append([]finding{}, env.Findings...), env.Failures...)
		if len(items) == 0 {
			for _, r := range env.Results {
				if (r.Healthy != nil && !*r.Healthy) || strings.EqualFold(r.Status, "failed") || strings.EqualFold(r.Status, "unhealthy") {
					items = append(items, r)
				}
			}
		}
		for _, item := range items {
			obj := firstNonEmpty(item.Object, item.Host, item.Node, scope, "cluster")
			key := firstNonEmpty(item.FaultKey, item.Check, "fault_check")
			out = append(out, FaultSegmentInput{
				FaultKey:  key,
				ObjectRef: obj,
				FieldKey:  "fault_check",
				Value:     firstNonEmpty(item.Detail, item.Status, "unhealthy"),
				SourceRef: sourceRef,
				State:     "active",
			})
		}
	}
	if len(out) == 0 && res.IsError {
		out = append(out, FaultSegmentInput{
			FaultKey:  "fault_check_error",
			ObjectRef: firstNonEmpty(strings.TrimSpace(scope), "cluster"),
			FieldKey:  "fault_check",
			Value:     firstNonEmpty(strings.TrimSpace(res.Text), "error"),
			SourceRef: sourceRef,
			State:     "active",
		})
	}
	return out
}
