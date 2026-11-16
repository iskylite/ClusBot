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
	State     string `yaml:"state"`
	Read      bool   `yaml:"allow_read"`
	Repair    bool   `yaml:"allow_repair"`
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
