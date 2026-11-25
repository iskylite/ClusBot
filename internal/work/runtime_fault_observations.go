package work

import (
	"context"
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

// SyncFaultSegmentsFromMCP explicitly checks named nodes with a bound NHC.
// An execution or coverage failure is not a fault finding or a healthy node.
// Attributable faults may persist even if another target was not observed.
func (s *Store) SyncFaultSegmentsFromMCP(ctx context.Context, scope string) ([]FaultSegmentSummary, error) {
	nodes := strings.Split(scope, ",")
	if len(nodes) > 32 {
		return nil, fmt.Errorf("%w：一次最多明确指定 32 个节点", ErrHealthCoverage)
	}
	seen := make(map[string]bool, len(nodes))
	for i, node := range nodes {
		node = strings.TrimSpace(node)
		if !healthNodeName.MatchString(node) || seen[node] {
			return nil, fmt.Errorf("%w：请提供不重复的具体节点名称，以逗号分隔", ErrHealthCoverage)
		}
		seen[node], nodes[i] = true, node
	}
	res, binding, bound, err := s.callBoundNHC(ctx, "ops.fault_check", nodes, false)
	if err != nil {
		return nil, err
	}
	if !bound {
		return nil, fmt.Errorf("%w：未配置只读 ops.fault_check → nhc", ErrCapabilityBinding)
	}
	report := parseMCPNHCResult(res, nodes)
	source := "mcp:" + binding.ConnectionID + ":nhc:" + report.TaskID
	out := make([]FaultSegmentSummary, 0)
	for _, node := range nodes {
		if report.Nodes[node] != mcpNHCFault {
			continue
		}
		item, err := s.RecordFaultSegment(ctx, FaultSegmentInput{
			FaultKey: "nhc", ObjectRef: node, FieldKey: "fault_status",
			Value: mcpNHCFault, SourceRef: source, State: "active",
		})
		if err != nil {
			return out, err
		}
		out = append(out, item)
	}
	if !report.Complete {
		var missing []string
		for _, node := range nodes {
			if report.Nodes[node] == mcpNHCUnknown {
				missing = append(missing, node)
			}
		}
		list := strings.Join(missing[:min(len(missing), 3)], "、")
		if len(missing) > 3 {
			list += fmt.Sprintf("等 %d 个节点", len(missing))
		}
		return out, fmt.Errorf("%w：未能核实 %s；本次已保存 %d 条可归属故障", ErrHealthCoverage, list, len(out))
	}
	return out, nil
}
