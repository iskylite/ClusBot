package work

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type DomainAssetEntry struct {
	ObjectRef string `yaml:"object_ref"`
	Kind      string `yaml:"kind"`
	Identity  string `yaml:"identity"`
	// Optional instance-wide hardware serial, supplied by the inventory
	// operator. Local aliases and discovery-source identities are not equivalent.
	PhysicalSerial string                 `yaml:"physical_serial"`
	State          string                 `yaml:"state"`
	PodRef         string                 `yaml:"pod_ref"`
	Read           bool                   `yaml:"allow_read"`
	Repair         bool                   `yaml:"allow_repair"`
	Benchmark      []DomainBenchmarkGrant `yaml:"benchmark_grants"`
}

// DomainBenchmarkGrant is an explicit local operator grant, never derived
// from a published plan, a provider claim, or a user message.
type DomainBenchmarkGrant struct {
	Revision   int                 `yaml:"revision" json:"revision"`
	Connection string              `yaml:"connection" json:"connection"`
	Tool       string              `yaml:"tool" json:"tool"`
	Allowed    map[string][]string `yaml:"allowed" json:"allowed"`
	ValidFrom  *int64              `yaml:"valid_from_unix_ms,omitempty" json:"-"`
	ExpiresAt  int64               `yaml:"expires_at_unix_ms" json:"-"`
}

// ImportDomainAssetsForAccount installs operator-provided inventory into the
// explicitly selected, already-bound account store, never the instance DB.
func (s *Store) ImportDomainAssetsForAccount(ctx context.Context, principal string, entries []DomainAssetEntry) error {
	key, ok := strings.CutPrefix(principal, "user:")
	if !ok || len(key) != 32 || strings.Trim(key, "0123456789abcdef") != "" {
		return ErrAuthorizationDenied
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	owned, err := ownsPersonalBusiness(tx, principal)
	if err != nil {
		return err
	}
	if !owned {
		return ErrAuthorizationDenied
	}
	if err := importDomainAssets(ctx, tx, principal, entries); err != nil {
		return err
	}
	return tx.Commit()
}

func importDomainAssets(ctx context.Context, tx *sql.Tx, principal string, entries []DomainAssetEntry) error {
	if len(entries) == 0 {
		return errors.New("清单须包含资产")
	}
	stamp := now()
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		ref := strings.TrimSpace(entry.ObjectRef)
		if ref == "" || len(ref) > 256 || seen[ref] || strings.TrimSpace(entry.Kind) == "" || strings.TrimSpace(entry.Identity) == "" {
			return errors.New("资产清单含空标识、重复标识或缺少类型和身份")
		}
		seen[ref] = true
		existing, err := readAsset(tx, ref)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			state := entry.State
			if state == "" {
				state = AssetStateRecheck
			}
			if _, known := assetTransitions[state]; !known {
				return fmt.Errorf("资产 %s 状态无效", ref)
			}
			if _, err := upsertAsset(tx, OpsAsset{ObjectRef: ref, Kind: entry.Kind, Identity: entry.Identity, State: state, CurrentOwner: principal}, stamp); err != nil {
				return err
			}
		case err != nil:
			return err
		case existing.Identity != entry.Identity || existing.Kind != entry.Kind:
			return fmt.Errorf("%w：资产 %s 的身份或类型与现有记录不符", ErrConflict, ref)
		}
		if entry.PhysicalSerial != "" {
			if err := bindBenchmarkPhysicalSerial(ctx, tx, ref, entry.PhysicalSerial); err != nil {
				return err
			}
		}
		for _, grant := range entry.Benchmark {
			if grant.Revision < 1 || !validBenchKey(grant.Connection) || !validBenchKey(grant.Tool) || grant.ExpiresAt <= stamp || len(grant.Allowed) > 32 {
				return ErrBenchmarkInput
			}
			// Optional operator windows narrow this grant only. Import does
			// not reserve resources, schedule work, or revoke other grants.
			validFrom := stamp
			if grant.ValidFrom != nil {
				validFrom = *grant.ValidFrom
				if validFrom <= 0 || validFrom >= grant.ExpiresAt {
					return fmt.Errorf("%w：基准授权开始时间必须为正且早于到期时间", ErrBenchmarkInput)
				}
			}
			for k, values := range grant.Allowed {
				if !validBenchKey(k) || len(values) == 0 || len(values) > 64 {
					return ErrBenchmarkInput
				}
				for _, value := range values {
					if value == "" || len(value) > 256 || strings.TrimSpace(value) != value {
						return ErrBenchmarkInput
					}
				}
			}
			params := toJSON(grant)
			var exists bool
			if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM authorizations WHERE subject=? AND action='benchmark.execute'
AND object_ref=? AND param_constraints=? AND expires_at=? AND revoked_at=0
AND ((? AND valid_from=created_at) OR (NOT ? AND valid_from=?)))`, principal, ref, params, grant.ExpiresAt,
				grant.ValidFrom == nil, grant.ValidFrom == nil, validFrom).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				if _, err := grantAuthorization(tx, Authorization{Subject: principal, Action: "benchmark.execute", ObjectRef: ref,
					ParamConstraints: params, ExpiresAt: grant.ExpiresAt, ValidFrom: validFrom, Source: "local-domain-asset-import", Owner: principal}, stamp); err != nil {
					return err
				}
			}
		}

		for _, access := range []struct {
			action, object string
			allowed        bool
		}{
			{"read", ref, entry.Read}, {"repair", ref, entry.Repair}, {"write", ref, entry.Repair}, {"dispatch", "ticket.create", entry.Repair},
		} {
			if !access.allowed {
				continue
			}
			var exists bool
			if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM authorizations WHERE subject=? AND action=? AND object_ref=?
AND revoked_at=0 AND valid_from<=? AND (expires_at=0 OR expires_at>?))`, principal, access.action, access.object, stamp, stamp).Scan(&exists); err != nil {
				return err
			}
			if exists {
				continue
			}
			if _, err := grantAuthorization(tx, Authorization{Subject: principal, Action: access.action, ObjectRef: access.object,
				Source: "local-domain-asset-import", Owner: principal, ValidFrom: stamp}, stamp); err != nil {
				return err
			}
		}
	}
	// Resolve POD references only after every entry has been registered, so a
	// manifest can place a node before its parent. Omitting pod_ref leaves any
	// existing relationship untouched; moving a node requires explicit review.
	for _, entry := range entries {
		if entry.PodRef == "" {
			continue
		}
		ref, podRef := strings.TrimSpace(entry.ObjectRef), entry.PodRef
		if strings.TrimSpace(podRef) != podRef || len(podRef) > 256 || podRef == ref {
			return fmt.Errorf("资产 %s 的 pod_ref 无效", ref)
		}
		node, err := readAsset(tx, ref)
		if err != nil {
			return err
		}
		if node.Kind != "node" && node.Kind != "计算节点" && node.Kind != "GPU 节点" {
			return fmt.Errorf("资产 %s 不是可归属 POD 的节点", ref)
		}
		pod, err := readAsset(tx, podRef)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("资产 %s 引用的 POD 未登记", ref)
		}
		if err != nil {
			return err
		}
		if pod.Kind != "pod" {
			return fmt.Errorf("资产 %s 引用的对象不是 pod 类型", ref)
		}
		rows, err := tx.QueryContext(ctx, `SELECT r.source_ref,r.relation_kind,r.valid_from,COALESCE(p.kind,'')
			FROM relations r LEFT JOIN ops_assets p ON p.object_ref=r.source_ref
			WHERE r.target_ref=? AND r.relation_kind IN ('physical_parent','physical_component')
			AND (r.valid_to=0 OR r.valid_to>?)`, ref, stamp)
		if err != nil {
			return err
		}
		present := false
		for rows.Next() {
			var parent, kind, parentKind string
			var from int64
			if err := rows.Scan(&parent, &kind, &from, &parentKind); err != nil {
				rows.Close()
				return err
			}
			if (kind == "physical_parent" || parentKind == "pod") && parent != podRef {
				rows.Close()
				return fmt.Errorf("%w：资产 %s 已有冲突的物理父关系", ErrConflict, ref)
			}
			if kind == "physical_parent" && parent == podRef {
				if from > stamp {
					rows.Close()
					return fmt.Errorf("%w：资产 %s 的 POD 关系尚未生效", ErrConflict, ref)
				}
				present = true
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if !present {
			if _, err := assertRelation(tx, Relation{RelationKind: "physical_parent", SourceRef: podRef, TargetRef: ref, ValidFrom: stamp}, stamp); err != nil {
				return err
			}
		}
	}
	return nil
}
