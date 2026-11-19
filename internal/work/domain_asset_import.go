package work

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type DomainAssetEntry struct {
	ObjectRef string                 `yaml:"object_ref"`
	Kind      string                 `yaml:"kind"`
	Identity  string                 `yaml:"identity"`
	State     string                 `yaml:"state"`
	Read      bool                   `yaml:"allow_read"`
	Repair    bool                   `yaml:"allow_repair"`
	Benchmark []DomainBenchmarkGrant `yaml:"benchmark_grants"`
}

// DomainBenchmarkGrant is an explicit local operator grant, never derived
// from a published plan, a provider claim, or a user message.
type DomainBenchmarkGrant struct {
	Revision   int                 `yaml:"revision" json:"revision"`
	Connection string              `yaml:"connection" json:"connection"`
	Tool       string              `yaml:"tool" json:"tool"`
	Allowed    map[string][]string `yaml:"allowed" json:"allowed"`
	ExpiresAt  int64               `yaml:"expires_at_unix_ms" json:"-"`
}

// ImportDomainAssets installs an operator-managed inventory into a local data directory.
func (s *Store) ImportDomainAssets(ctx context.Context, entries []DomainAssetEntry) error {
	if len(entries) == 0 {
		return errors.New("清单须包含资产")
	}
	const principal = "user:session"
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
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
		for _, grant := range entry.Benchmark {
			if grant.Revision < 1 || !validBenchKey(grant.Connection) || !validBenchKey(grant.Tool) || grant.ExpiresAt <= stamp || len(grant.Allowed) > 32 {
				return ErrBenchmarkInput
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
AND object_ref=? AND param_constraints=? AND expires_at=? AND revoked_at=0)`, principal, ref, params, grant.ExpiresAt).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				if _, err := grantAuthorization(tx, Authorization{Subject: principal, Action: "benchmark.execute", ObjectRef: ref,
					ParamConstraints: params, ExpiresAt: grant.ExpiresAt, ValidFrom: stamp, Source: "local-domain-asset-import", Owner: principal}, stamp); err != nil {
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
	return tx.Commit()
}
