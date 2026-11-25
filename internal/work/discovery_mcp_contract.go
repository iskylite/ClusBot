package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"clusbot/internal/mcpclient"
)

// DiscoveryScopeSpec distinguishes the user-visible scope label from the
// actual bounded scan. Only configured, reviewed inputs are ever dispatched.
type DiscoveryScopeSpec struct {
	Protocol  string   `json:"protocol"`
	Nodes     string   `json:"nodes,omitempty"`
	IPRange   []string `json:"ip_range,omitempty"`
	BMCRange  []string `json:"bmc_range,omitempty"`
	BMCMap    []string `json:"bmc_map,omitempty"`
	BMCSuffix bool     `json:"bmc_suffix,omitempty"`
}

type discoverySourceBinding struct {
	ConnectionID string             `json:"connection_id"`
	Revision     int                `json:"revision"`
	Tool         string             `json:"tool"`
	Versions     map[string]string  `json:"versions"`
	Scan         DiscoveryScopeSpec `json:"scan"`
}

func (b *discoverySourceBinding) fingerprint() string {
	if b == nil {
		return ""
	}
	return digest(toJSON(b))
}

type discoveryScanCoverage struct {
	Outcome            string   `json:"outcome"`
	Total              int      `json:"total"`
	Observed           int      `json:"observed"`
	UnconfirmedTargets []string `json:"unconfirmed_targets,omitempty"`
}

type scopedDiscoverySource interface {
	pinDiscovery(context.Context, string, string, *DiscoveryScopeSpec) (*discoverySourceBinding, error)
	scanPinnedDiscovery(context.Context, string, string, *discoverySourceBinding, string) ([]DiscoveryObservation, *discoveryScanCoverage, error)
}

type guardedDiscoverySource struct {
	DiscoveryScanSource
	check func(context.Context) error
}

func GuardDiscoverySource(base DiscoveryScanSource, check func(context.Context) error) DiscoveryScanSource {
	if base == nil || check == nil {
		return base
	}
	return guardedDiscoverySource{DiscoveryScanSource: base, check: check}
}

func (s guardedDiscoverySource) ScanDiscovery(ctx context.Context, source, scope string) ([]DiscoveryObservation, error) {
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	return s.DiscoveryScanSource.ScanDiscovery(ctx, source, scope)
}
func (s guardedDiscoverySource) pinDiscovery(ctx context.Context, source, scope string, args *DiscoveryScopeSpec) (*discoverySourceBinding, error) {
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	return pinDiscoverySource(ctx, s.DiscoveryScanSource, source, scope, args)
}
func (s guardedDiscoverySource) scanPinnedDiscovery(ctx context.Context, source, scope string, bind *discoverySourceBinding, runKey string) ([]DiscoveryObservation, *discoveryScanCoverage, error) {
	if err := s.check(ctx); err != nil {
		return nil, nil, err
	}
	scoped, ok := s.DiscoveryScanSource.(scopedDiscoverySource)
	if !ok {
		if bind != nil {
			return nil, nil, ErrDiscoveryInput
		}
		obs, err := s.DiscoveryScanSource.ScanDiscovery(ctx, source, scope)
		return obs, nil, err
	}
	obs, coverage, err := scoped.scanPinnedDiscovery(ctx, source, scope, bind, runKey)
	if err != nil {
		return nil, nil, err
	}
	if err = s.check(ctx); err != nil {
		return nil, nil, err
	}
	return obs, coverage, nil
}
func pinDiscoverySource(ctx context.Context, source DiscoveryScanSource, id, scope string, args *DiscoveryScopeSpec) (*discoverySourceBinding, error) {
	if source == nil || !validDiscoveryLabel(id) || !validDiscoveryLabel(scope) {
		return nil, ErrDiscoveryInput
	}
	if scoped, ok := source.(scopedDiscoverySource); ok {
		return scoped.pinDiscovery(ctx, id, scope, args)
	}
	if args != nil || !source.AllowsDiscovery(id, scope) {
		return nil, ErrDiscoveryInput
	}
	return nil, nil
}
func validDiscoveryLabel(v string) bool {
	return v != "" && len(v) <= 256 && strings.TrimSpace(v) == v && !strings.ContainsAny(v, "\x00\r\n\t")
}

var discoveryNodeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
var discoveryNodeSet = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*\[([0-9]{1,8}(?:-[0-9]{1,8})?(?:,[0-9]{1,8}(?:-[0-9]{1,8})?)*)\][A-Za-z0-9_.-]*$`)

func boundedDiscoveryNodes(raw string) bool {
	if len(raw) > 4096 {
		return false
	}
	var parts []string
	begin, depth := 0, 0
	for i, ch := range raw {
		switch ch {
		case '[':
			depth++
		case ']':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, raw[begin:i])
				begin = i + 1
			}
		}
		if depth < 0 || depth > 1 {
			return false
		}
	}
	if depth != 0 {
		return false
	}
	parts = append(parts, raw[begin:])
	count := 0
	for _, rawPart := range parts {
		part := strings.TrimSpace(rawPart)
		if discoveryNodeName.MatchString(part) || isDiscoveryIPv4(part) {
			count++
			continue
		}
		m := discoveryNodeSet.FindStringSubmatch(part)
		if len(m) != 2 {
			return false
		}
		for _, group := range strings.Split(m[1], ",") {
			first, last, rangeSet := strings.Cut(group, "-")
			start, _ := strconv.Atoi(first)
			end := start
			if rangeSet {
				end, _ = strconv.Atoi(last)
			}
			if end < start || end-start >= 256 {
				return false
			}
			count += end - start + 1
			if count > 256 {
				return false
			}
		}
	}
	return count > 0 && count <= 256
}

func isDiscoveryIPv4(v string) bool {
	ip, err := netip.ParseAddr(v)
	return err == nil && ip.Is4()
}

func countDiscoveryRange(s string) (uint64, bool) {
	if prefix, err := netip.ParsePrefix(s); err == nil && prefix.Addr().Is4() && prefix == prefix.Masked() {
		count := uint64(1) << uint(32-prefix.Bits())
		if prefix.Bits() <= 30 {
			count -= 2
		}
		return count, true
	}
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return 0, false
	}
	ip1, e1 := netip.ParseAddr(a)
	ip2, e2 := netip.ParseAddr(b)
	if e1 != nil || e2 != nil || !ip1.Is4() || !ip2.Is4() || ip1.Compare(ip2) > 0 {
		return 0, false
	}
	number := func(ip netip.Addr) uint64 {
		x := ip.As4()
		return uint64(x[0])<<24 | uint64(x[1])<<16 | uint64(x[2])<<8 | uint64(x[3])
	}
	return number(ip2) - number(ip1) + 1, true
}

func normalizeDiscoveryScope(label string, input *DiscoveryScopeSpec) (DiscoveryScopeSpec, error) {
	scan := DiscoveryScopeSpec{Protocol: "ssh"}
	if input == nil {
		if _, ok := countDiscoveryRange(label); ok {
			scan.IPRange = []string{label}
		} else if label != "default" && label != "all" {
			scan.Nodes = label
		}
	} else {
		scan = *input
		scan.IPRange = append([]string(nil), input.IPRange...)
		scan.BMCRange = append([]string(nil), input.BMCRange...)
		scan.BMCMap = append([]string(nil), input.BMCMap...)
		if scan.Protocol == "" {
			scan.Protocol = "ssh"
		}
	}
	if scan.Protocol != "ssh" && scan.Protocol != "redfish" && scan.Protocol != "both" {
		return scan, ErrDiscoveryInput
	}
	if (scan.Nodes == "") == (len(scan.IPRange) == 0) || scan.Nodes != "" && !boundedDiscoveryNodes(scan.Nodes) ||
		len(scan.BMCRange) > 0 && (scan.Protocol != "both" || len(scan.IPRange) == 0 || len(scan.BMCMap) > 0) ||
		len(scan.BMCMap) > 0 && scan.Protocol == "ssh" ||
		scan.BMCSuffix && scan.Protocol != "redfish" ||
		scan.Protocol == "both" && len(scan.IPRange) > 0 && len(scan.BMCRange) == 0 && len(scan.BMCMap) == 0 {
		return scan, ErrDiscoveryInput
	}
	total := uint64(0)
	for _, ranges := range [][]string{scan.IPRange, scan.BMCRange} {
		if len(ranges) > 256 {
			return scan, ErrDiscoveryInput
		}
		seen := map[string]bool{}
		for _, v := range ranges {
			count, ok := countDiscoveryRange(v)
			if !ok || count == 0 || count > 256 || seen[v] {
				return scan, ErrDiscoveryInput
			}
			total += count
			seen[v] = true
			if total > 256 {
				return scan, ErrDiscoveryInput
			}
		}
		sort.Strings(ranges)
	}
	if len(scan.BMCMap) > 256 {
		return scan, ErrDiscoveryInput
	}
	seen := map[string]bool{}
	for _, mapping := range scan.BMCMap {
		node, endpoint, ok := strings.Cut(mapping, "=")
		if !ok || !discoveryNodeName.MatchString(node) || seen[node] || !validObservedEndpoint(endpoint) {
			return scan, ErrDiscoveryInput
		}
		seen[node] = true
	}
	sort.Strings(scan.BMCMap)
	return scan, nil
}

func (s DiscoveryScopeSpec) toolArgs() map[string]any {
	args := map[string]any{"protocol": s.Protocol}
	if s.Nodes != "" {
		args["nodes"] = s.Nodes
	}
	if len(s.IPRange) > 0 {
		args["ip_range"] = s.IPRange
	}
	if len(s.BMCRange) > 0 {
		args["bmc_range"] = s.BMCRange
	}
	if len(s.BMCMap) > 0 {
		args["bmc_map"] = s.BMCMap
	}
	if s.BMCSuffix {
		args["bmc_suffix"] = true
	}
	return args
}

func (d *dynamicDiscoverySource) pinDiscovery(ctx context.Context, source, scope string, args *DiscoveryScopeSpec) (*discoverySourceBinding, error) {
	conn, name, ok := d.findMCPConnection(source)
	if !ok {
		if args == nil && d.fallback != nil && d.fallback.AllowsDiscovery(source, scope) {
			return nil, nil
		}
		return nil, ErrDiscoveryInput
	}
	if name != "discover" {
		return nil, fmt.Errorf("%w：资产发现须绑定已核验的 discover 接口", ErrDiscoveryInput)
	}
	scan, err := normalizeDiscoveryScope(scope, args)
	if err != nil {
		return nil, fmt.Errorf("%w：需明确有效的节点、CIDR 或地址范围，最多 256 个目标", err)
	}
	full, err := d.store.Connection(ctx, conn.ID)
	if err != nil || full.Revision != conn.Revision || full.Status != "ready" || !full.Enabled {
		return nil, ErrDiscoveryInput
	}
	versions := map[string]string{}
	allowed := map[string]mcpclient.Tool{}
	for _, tool := range full.Tools {
		if tool.Allowed && tool.ReadOnly && tool.ServerReadOnly != nil && *tool.ServerReadOnly {
			allowed[tool.Name] = tool
		}
	}
	for _, n := range []string{"discover", "task_status", "task_stream"} {
		tool := allowed[n]
		if tool.Name == "" {
			return nil, fmt.Errorf("%w：请授权 MCP 只读接口 %s", ErrDiscoveryInput, n)
		}
		versions[n] = tool.Version
	}
	toValidate := scan.toolArgs()
	toValidate["sync"] = false
	toValidate["idempotency_key"] = "discovery:preflight"
	if err = mcpclient.ValidateArguments(allowed["discover"], toValidate); err != nil {
		return nil, fmt.Errorf("%w：MCP discover 不支持已选范围", ErrDiscoveryInput)
	}
	for n, shape := range map[string]map[string]any{
		"task_status": {"task_id": "preflight"},
		"task_stream": {"task_id": "preflight", "from_seq": 1, "limit": 1, "event_filter": "result"},
	} {
		if err = mcpclient.ValidateArguments(allowed[n], shape); err != nil {
			return nil, fmt.Errorf("%w：MCP %s 不支持所需查询", ErrDiscoveryInput, n)
		}
	}
	return &discoverySourceBinding{ConnectionID: conn.ID, Revision: conn.Revision, Tool: name, Versions: versions, Scan: scan}, nil
}

func (d *dynamicDiscoverySource) scanPinnedDiscovery(ctx context.Context, source, scope string, b *discoverySourceBinding, runKey string) ([]DiscoveryObservation, *discoveryScanCoverage, error) {
	if b == nil {
		if d.fallback == nil || !d.fallback.AllowsDiscovery(source, scope) {
			return nil, nil, fmt.Errorf("%w：旧发现任务没有固定 MCP 连接，须重新授权发布", ErrDiscoveryInput)
		}
		obs, err := d.fallback.ScanDiscovery(ctx, source, scope)
		return obs, nil, err
	}
	actual, err := d.store.Connection(ctx, b.ConnectionID)
	if err != nil || actual.Revision != b.Revision || !actual.Enabled || actual.Status != "ready" {
		return nil, nil, ErrConnectionRevision
	}
	for name, version := range b.Versions {
		found := false
		for _, tool := range actual.Tools {
			if tool.Name == name && tool.Version == version && tool.Allowed && tool.ReadOnly && tool.ServerReadOnly != nil && *tool.ServerReadOnly {
				found = true
				break
			}
		}
		if !found {
			return nil, nil, ErrToolPermission
		}
	}
	call := func(name string, args map[string]any, target any) error {
		out, err := d.store.CallConnectionTool(ctx, b.ConnectionID, name, b.Revision, args)
		if err != nil {
			return err
		}
		if out.SourceUnavailable {
			return ErrAuthorizationDenied
		}
		if out.IsError {
			return fmt.Errorf("%w：MCP %s 未返回可用结果", ErrDiscoveryInput, name)
		}
		res := out.Result
		if len(res.Structured) == 0 && res.Reference != nil {
			raw, readErr := d.store.ConnectionEvidence(ctx, res.Reference.ID)
			if readErr != nil {
				return readErr
			}
			if len(raw) != res.Reference.Bytes || digest(string(raw)) != res.Reference.SHA256 {
				return ErrDiscoveryInput
			}
			if err := json.Unmarshal(raw, &res); err != nil {
				return err
			}
		}
		if len(res.Structured) == 0 {
			return fmt.Errorf("%w：缺少完整结构化 MCP 结果", ErrDiscoveryInput)
		}
		return json.Unmarshal(res.Structured, target)
	}
	args := b.Scan.toolArgs()
	args["sync"] = false
	args["idempotency_key"] = "discovery:" + digest(b.fingerprint()+"\x00"+runKey)
	var receipt struct {
		ID string `json:"task_id"`
	}
	if err = call(b.Tool, args, &receipt); err != nil {
		return nil, nil, err
	}
	if !validBenchKey(receipt.ID) {
		return nil, nil, ErrDiscoveryInput
	}
	var targetCount int
	for {
		var status struct {
			ID    string `json:"task_id"`
			State string `json:"state"`
			Total int    `json:"total"`
		}
		if err = call("task_status", map[string]any{"task_id": receipt.ID}, &status); err != nil {
			return nil, nil, err
		}
		if status.ID != receipt.ID {
			return nil, nil, ErrDiscoveryInput
		}
		switch strings.ToUpper(status.State) {
		case "COMPLETE":
			if status.Total < 1 || status.Total > 256 {
				return nil, nil, ErrDiscoveryInput
			}
			targetCount = status.Total
		case "RUNNING", "ACTIVE", "QUEUED":
		default:
			return nil, nil, fmt.Errorf("%w：原发现任务状态 %q", ErrDiscoveryInput, status.State)
		}
		if targetCount > 0 {
			break
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	var observed []DiscoveryObservation
	scopePart := false
	unconfirmed := map[string]bool{}
	rows := 0
	cursor := uint64(1)
	for pages := 0; pages < 2048 && rows < targetCount; pages++ {
		var page struct {
			ID     string `json:"task_id"`
			Next   uint64 `json:"next_seq"`
			Events []struct {
				Seq  uint64 `json:"seq"`
				Task string `json:"task_id"`
				Data struct {
					Asset *discoveryInventoryRow `json:"asset_discovery"`
				} `json:"data"`
			} `json:"events"`
		}
		if err = call("task_stream", map[string]any{"task_id": receipt.ID, "from_seq": cursor, "limit": 1, "event_filter": "result"}, &page); err != nil {
			return nil, nil, err
		}
		if page.ID != receipt.ID || page.Next <= cursor {
			return nil, nil, ErrDiscoveryInput
		}
		for _, event := range page.Events {
			if event.Task != receipt.ID || event.Seq < cursor || event.Seq >= page.Next {
				return nil, nil, ErrDiscoveryInput
			}
			if event.Data.Asset == nil {
				continue
			}
			row := event.Data.Asset
			rows++
			if row.Status != "succeeded" {
				scopePart = true
			}
			if o, ok := row.observation("mcp:"+b.ConnectionID, receipt.ID, event.Seq); ok {
				observed = append(observed, o)
				if row.Status != "succeeded" && validObservedEndpoint(row.Target) {
					unconfirmed[row.Target] = true
				}
			} else if validObservedEndpoint(row.Target) {
				unconfirmed[row.Target] = true
			}
		}
		cursor = page.Next
	}
	if rows != targetCount {
		return nil, nil, fmt.Errorf("%w：发现记录未完整读回", ErrDiscoveryInput)
	}
	outcome := "complete"
	if scopePart || len(observed) != targetCount {
		outcome = "partial"
	}
	var unknown []string
	for target := range unconfirmed {
		unknown = append(unknown, target)
	}
	sort.Strings(unknown)
	return observed, &discoveryScanCoverage{Outcome: outcome, Total: targetCount, Observed: len(observed), UnconfirmedTargets: unknown}, nil
}

func migrateDiscoveryMCPContract(tx *sql.Tx) error {
	_, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS discovery_plan_bindings (
 plan_id TEXT PRIMARY KEY REFERENCES discovery_plans(plan_id), binding TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS discovery_scan_coverages (
 plan_id TEXT NOT NULL, scheduled_at INTEGER NOT NULL, coverage TEXT NOT NULL,
 PRIMARY KEY(plan_id,scheduled_at)
);
CREATE TABLE IF NOT EXISTS discovery_observation_evidence (
 plan_id TEXT NOT NULL, scheduled_at INTEGER NOT NULL, ordinal INTEGER NOT NULL,
 evidence TEXT NOT NULL, fingerprint TEXT NOT NULL,
 PRIMARY KEY(plan_id,scheduled_at,ordinal)
);
CREATE TABLE IF NOT EXISTS discovery_admission_evidence (
 candidate_id TEXT PRIMARY KEY REFERENCES discovery_candidates(candidate_id),
 evidence TEXT NOT NULL, fingerprint TEXT NOT NULL
);`)
	return err
}

var _ scopedDiscoverySource = (*dynamicDiscoverySource)(nil)
var _ scopedDiscoverySource = guardedDiscoverySource{}

type discoveryInventoryRow struct {
	Target             string                                      `json:"target"`
	Status             string                                      `json:"status"`
	ObservedAt         time.Time                                   `json:"observed_at"`
	Observation        *struct{ Serial, Address string }           `json:"observation"`
	IdentityConflict   bool                                        `json:"identity_conflict"`
	ManagementEndpoint string                                      `json:"management_endpoint"`
	SSH                *struct{ Hostname, Address, Serial string } `json:"ssh"`
	Redfish            *struct {
		Assets             []struct{ Kind, Serial string } `json:"assets"`
		IdentityIncomplete bool                            `json:"identity_incomplete"`
	} `json:"redfish"`
}

func validObservedEndpoint(v string) bool {
	if len(v) == 0 || len(v) > 255 {
		return false
	}
	if u, err := netip.ParseAddrPort(v); err == nil {
		return u.Addr().Zone() == "" && u.Port() != 0
	}
	if a, err := netip.ParseAddr(v); err == nil {
		return a.Zone() == ""
	}
	if discoveryNodeName.MatchString(v) {
		return true
	}
	host, port, ok := strings.Cut(v, ":")
	n, err := strconv.ParseUint(port, 10, 16)
	return ok && err == nil && n != 0 && discoveryNodeName.MatchString(host)
}

func (row discoveryInventoryRow) observation(source, task string, sequence uint64) (DiscoveryObservation, bool) {
	if row.Observation == nil || row.IdentityConflict || (row.Status != "succeeded" && row.Status != "partial") {
		return DiscoveryObservation{}, false
	}
	serial := row.Observation.Serial
	address, err := netip.ParseAddr(row.Observation.Address)
	if err != nil || address.Zone() != "" || serial == "" || serial != strings.TrimSpace(serial) || len(serial) > 128 {
		return DiscoveryObservation{}, false
	}
	o := DiscoveryObservation{Serial: serial, Address: address.Unmap().String(), SourceIdentity: source, ProviderTaskID: task, Sequence: sequence, ObservedAt: row.ObservedAt, ProviderTarget: row.Target}
	if row.SSH != nil && strings.EqualFold(row.SSH.Serial, serial) {
		o.Kind = "node"
		if validObservedEndpoint(row.Target) {
			o.SSHEndpoint = row.Target
		} else if validObservedEndpoint(row.SSH.Address) {
			o.SSHEndpoint = row.SSH.Address
		}
	}
	if row.Redfish != nil && !row.Redfish.IdentityIncomplete && len(row.Redfish.Assets) == 1 &&
		strings.EqualFold(row.Redfish.Assets[0].Serial, serial) && validObservedEndpoint(row.ManagementEndpoint) {
		o.BMCEndpoint = row.ManagementEndpoint
		kind := row.Redfish.Assets[0].Kind
		if kind == "server" {
			kind = "node"
		}
		if kind != "node" && kind != "switch" && kind != "chassis" {
			return DiscoveryObservation{}, false
		}
		if o.Kind != "" && o.Kind != kind {
			return DiscoveryObservation{}, false
		}
		o.Kind = kind
	}
	return o, o.Kind != "" && (o.SSHEndpoint != "" || o.BMCEndpoint != "")
}
