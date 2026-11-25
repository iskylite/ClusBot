package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"

	"clusbot/internal/mcpclient"
)

type dynamicDiscoverySource struct {
	store    *Store
	fallback DiscoveryScanSource
}

// DynamicDiscoverySource resolves MCP `discover` capability bindings or allowed read-only
// `discover` tools at runtime, falling back to HTTP/static DiscoveryScanSource otherwise.
func (s *Store) DynamicDiscoverySource(fallback DiscoveryScanSource) DiscoveryScanSource {
	return &dynamicDiscoverySource{store: s, fallback: fallback}
}

func (d *dynamicDiscoverySource) readConnDB() *sql.DB {
	if d.store.readPool != nil && d.store.readPool.db != nil {
		return d.store.readPool.db
	}
	return d.store.db
}

func (d *dynamicDiscoverySource) findMCPConnection(sourceID string) (Connection, string, bool) {
	if d == nil || d.store == nil {
		return Connection{}, "", false
	}
	ctx := context.Background()
	rdb := d.readConnDB()
	if rdb == nil {
		return Connection{}, "", false
	}
	var connID, connName, toolName string
	var connRev int
	err := rdb.QueryRowContext(ctx, `SELECT c.id, c.name, c.revision, p.config_ref
FROM capabilities cap
JOIN capability_providers p ON p.kind='mcp' AND p.capabilities=json_array(cap.capability_key||':'||cap.version)
JOIN capability_connections cc ON cc.provider_id=p.provider_id AND cc.enabled=1
JOIN connections c ON c.id=cc.credential_ref AND c.enabled=1 AND c.status='ready'
WHERE (cap.capability_key='asset.discover' OR p.config_ref LIKE '%"start_tool":"discover"%')
  AND (?='' OR ?='mcp' OR c.id=? OR c.name=?)
LIMIT 1`, sourceID, sourceID, sourceID, sourceID).Scan(&connID, &connName, &connRev, &toolName)
	if err == nil && connID != "" {
		var cfg struct {
			StartTool string `json:"start_tool"`
		}
		_ = json.Unmarshal([]byte(toolName), &cfg)
		return Connection{ID: connID, Name: connName, Revision: connRev, Enabled: true, Status: "ready"}, firstNonEmpty(cfg.StartTool, "discover"), true
	}

	rows, err := rdb.QueryContext(ctx, `SELECT c.id, c.name, c.revision, t.definition
FROM connections c
JOIN connection_tools t ON t.connection_id=c.id
WHERE c.enabled=1 AND c.status='ready' AND t.name='discover' AND t.allowed=1
  AND (?='' OR ?='mcp' OR c.id=? OR c.name=?)`, sourceID, sourceID, sourceID, sourceID)
	if err != nil {
		return Connection{}, "", false
	}
	defer rows.Close()
	for rows.Next() {
		var def string
		if err := rows.Scan(&connID, &connName, &connRev, &def); err != nil {
			continue
		}
		var tool mcpclient.Tool
		if json.Unmarshal([]byte(def), &tool) == nil && !isToolDisallowedForReadOnly(tool) {
			return Connection{ID: connID, Name: connName, Revision: connRev, Enabled: true, Status: "ready"}, "discover", true
		}
	}
	return Connection{}, "", false
}

func (d *dynamicDiscoverySource) AllowsDiscovery(id, scope string) bool {
	if d.fallback != nil && d.fallback.AllowsDiscovery(id, scope) {
		return true
	}
	_, _, ok := d.findMCPConnection(id)
	return ok
}

func (d *dynamicDiscoverySource) ScanDiscovery(ctx context.Context, id, scope string) ([]DiscoveryObservation, error) {
	conn, toolName, ok := d.findMCPConnection(id)
	if !ok {
		if d.fallback != nil {
			return d.fallback.ScanDiscovery(ctx, id, scope)
		}
		return nil, ErrDiscoveryInput
	}
	var tool mcpclient.Tool
	if fullConn, connErr := d.store.Connection(ctx, conn.ID); connErr == nil {
		for _, t := range fullConn.Tools {
			if t.Name == toolName {
				tool = t
				break
			}
		}
	}
	props := toolSchemaProperties(tool)
	args := map[string]any{}
	if _, hasSync := props["sync"]; hasSync {
		args["sync"] = true
	}
	trimmed := strings.TrimSpace(scope)
	if trimmed != "" && looksLikeDiscoveryTarget(trimmed) {
		if _, hasIPRange := props["ip_range"]; hasIPRange && isIPRangeOrCIDR(trimmed) {
			args["ip_range"] = []string{trimmed}
		} else {
			args["nodes"] = trimmed
		}
	}
	res, err := d.store.CallConnectionTool(ctx, conn.ID, toolName, conn.Revision, args)
	if err != nil {
		return nil, err
	}
	if res.IsError {
		return nil, fmt.Errorf("mcp discover error: %s", res.Text)
	}
	return parseDiscoveryObservations(res.Result)
}

func isIPRangeOrCIDR(s string) bool {
	if _, err := netip.ParsePrefix(s); err == nil {
		return true
	}
	if start, end, ok := strings.Cut(s, "-"); ok {
		_, err1 := netip.ParseAddr(strings.TrimSpace(start))
		_, err2 := netip.ParseAddr(strings.TrimSpace(end))
		return err1 == nil && err2 == nil
	}
	return false
}

func looksLikeDiscoveryTarget(scope string) bool {
	s := strings.TrimSpace(scope)
	if s == "" || s == "default" || s == "all" {
		return false
	}
	if _, err := netip.ParseAddr(s); err == nil {
		return true
	}
	if _, err := netip.ParsePrefix(s); err == nil {
		return true
	}
	return strings.ContainsAny(s, ",.")
}

func parseDiscoveryObservations(res mcpclient.Result) ([]DiscoveryObservation, error) {
	raw := res.Structured
	if len(raw) == 0 {
		raw = []byte(strings.TrimSpace(res.Text))
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var direct []DiscoveryObservation
	if json.Unmarshal(raw, &direct) == nil && len(direct) > 0 && direct[0].Address != "" {
		return direct, nil
	}
	type cluskitObs struct {
		Host     string `json:"host"`
		Hostname string `json:"hostname"`
		Address  string `json:"address"`
		IP       string `json:"ip"`
		Serial   string `json:"serial"`
		BMCMAC   string `json:"bmc_mac"`
	}
	var env struct {
		Observations []cluskitObs `json:"observations"`
		Summary      struct {
			Observations []cluskitObs `json:"observations"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	items := env.Observations
	if len(items) == 0 {
		items = env.Summary.Observations
	}
	out := make([]DiscoveryObservation, 0, len(items))
	for _, item := range items {
		addr := firstNonEmpty(item.Address, item.IP, item.Host, item.Hostname)
		if addr == "" {
			continue
		}
		serial := firstNonEmpty(item.Serial, item.BMCMAC)
		out = append(out, DiscoveryObservation{
			Serial:  serial,
			Address: addr,
		})
	}
	return out, nil
}
