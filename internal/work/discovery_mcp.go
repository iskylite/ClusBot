package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
	rows, err := rdb.QueryContext(ctx, `SELECT c.id,c.name,c.revision,t.definition
FROM connections c JOIN connection_tools t ON t.connection_id=c.id
WHERE c.enabled=1 AND c.status='ready' AND t.name='discover' AND t.allowed=1 AND t.read_only=1
AND (?='mcp' OR c.id=? OR c.name=?) ORDER BY c.id`, sourceID, sourceID, sourceID)
	if err != nil {
		return Connection{}, "", false
	}
	defer rows.Close()
	var chosen Connection
	for rows.Next() {
		var conn Connection
		var definition string
		if err := rows.Scan(&conn.ID, &conn.Name, &conn.Revision, &definition); err != nil {
			return Connection{}, "", false
		}
		var tool mcpclient.Tool
		if json.Unmarshal([]byte(definition), &tool) != nil || isToolDisallowedForReadOnly(tool) {
			continue
		}
		if chosen.ID != "" {
			return Connection{}, "", false
		} // no ambiguous source alias
		chosen = conn
	}
	if rows.Err() != nil || chosen.ID == "" {
		return Connection{}, "", false
	}
	chosen.Enabled, chosen.Status = true, "ready"
	return chosen, "discover", true
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
	scan, err := normalizeDiscoveryScope(strings.TrimSpace(scope), nil)
	if err != nil {
		return nil, err
	}
	for name, value := range scan.toolArgs() {
		if _, supported := props[name]; supported {
			args[name] = value
		} else if name != "protocol" {
			return nil, ErrDiscoveryInput
		}
	}
	res, err := d.store.CallConnectionTool(ctx, conn.ID, toolName, conn.Revision, args)
	if err != nil {
		return nil, err
	}
	if res.SourceUnavailable {
		return nil, ErrAuthorizationDenied
	}
	if res.IsError {
		return nil, fmt.Errorf("mcp discover error: %s", res.Text)
	}
	return parseDiscoveryObservations(res.Result)
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
