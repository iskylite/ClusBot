package work

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

func bindBenchmarkPhysicalSerial(ctx context.Context, tx *sql.Tx, ref, input string) error {
	serial := strings.TrimSpace(input)
	var hasAlphanumeric bool
	for _, c := range serial {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			hasAlphanumeric = true
		} else if c != '.' && c != '_' && c != '-' {
			return fmt.Errorf("%w：资产 %s 的物理序列号含不支持的字符", ErrBenchmarkInput, ref)
		}
	}
	serial = strings.ToUpper(serial)
	switch serial {
	case "", "NA", "NONE", "UNKNOWN", "UNSPECIFIED", "DEFAULT", "NOTAPPLICABLE", "NOTSPECIFIED":
		return fmt.Errorf("%w：资产 %s 缺少可信的物理序列号", ErrBenchmarkInput, ref)
	}
	if len(serial) > 128 || !hasAlphanumeric || strings.Trim(serial, "0") == "" {
		return fmt.Errorf("%w：资产 %s 的物理序列号无效", ErrBenchmarkInput, ref)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO benchmark_asset_physical_serials VALUES(?,?) ON CONFLICT(target_ref) DO NOTHING", ref, serial); err != nil {
		return err
	}
	var saved string
	err := tx.QueryRowContext(ctx, "SELECT physical_serial FROM benchmark_asset_physical_serials WHERE target_ref=?", ref).Scan(&saved)
	if err == nil && serial != saved {
		return fmt.Errorf("%w：资产 %s 已绑定不同的物理序列号", ErrConflict, ref)
	}
	return err
}

type benchmarkHoldOwner struct {
	step, operation string
}

// Lock order must always be account writer, then instance writer. SQLite WAL
// cannot commit changes across two databases atomically. Therefore the instance
// hold commits first. Provider Start is permitted only after the account's
// operation/attempt commits. A crash at either boundary errs toward holding;
// recovery removes only holds whose private durable state proves no dispatch
// or confirms a provider terminal result. There is no TTL-based release.
func lockBenchmarkAccount(ctx context.Context, tx *sql.Tx) (string, error) {
	if _, err := tx.ExecContext(ctx, "UPDATE account_business_owner SET singleton=singleton WHERE singleton=1"); err != nil {
		return "", err
	}
	var principal string
	err := tx.QueryRowContext(ctx, "SELECT principal_id FROM account_business_owner WHERE singleton=1").Scan(&principal)
	return principal, err
}

func reapAccountBenchmarkHolds(ctx context.Context, tx *sql.Tx, instance *sql.DB, principal string) error {
	rows, err := instance.QueryContext(ctx, "SELECT DISTINCT step_id,operation_id FROM benchmark_instance_holds WHERE principal_id=?", principal)
	if err != nil {
		return err
	}
	var owners []benchmarkHoldOwner
	for rows.Next() {
		var owner benchmarkHoldOwner
		if err := rows.Scan(&owner.step, &owner.operation); err != nil {
			rows.Close()
			return err
		}
		owners = append(owners, owner)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, owner := range owners {
		var phase, operation, state string
		var generation int
		var localHold bool
		err := tx.QueryRowContext(ctx, `SELECT b.phase,b.operation_id,b.generation,coalesce(o.state,''),EXISTS(
SELECT 1 FROM benchmark_target_holds h WHERE h.step_id=b.step_id)
FROM benchmark_cases b LEFT JOIN operations o ON o.operation_id=b.operation_id AND o.step_id=b.step_id WHERE b.step_id=?`, owner.step).
			Scan(&phase, &operation, &generation, &state, &localHold)
		// Missing, mismatched, or unclear local evidence cannot release a hold.
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		neverDispatched := operation == "" && generation == 0 && (phase == "queued" || phase == "skipped")
		terminal := operation == owner.operation && (phase == "verified" || phase == "incomplete") &&
			(state == string(RuntimeOperationSucceeded) || state == string(RuntimeOperationFailed) || state == string(RuntimeOperationCancelled))
		if !localHold && (neverDispatched || terminal) {
			if _, err := instance.ExecContext(ctx, "DELETE FROM benchmark_instance_holds WHERE principal_id=? AND step_id=? AND operation_id=?", principal, owner.step, owner.operation); err != nil {
				return err
			}
		}
	}
	return nil
}

// Reserve the entire case or none of it. SQLite enforces exclusivity even
// when two account stores access the same instance DB through different handles.
func reserveInstanceBenchmarkHolds(ctx context.Context, instance *sql.DB, serials []string, principal, step, operation string) (bool, error) {
	if len(serials) == 0 {
		return false, ErrBenchmarkInput
	}
	tx, err := instance.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	for _, serial := range serials {
		row, err := tx.ExecContext(ctx, "INSERT INTO benchmark_instance_holds VALUES(?,?,?,?) ON CONFLICT(physical_serial) DO NOTHING", serial, principal, step, operation)
		if err != nil {
			return false, err
		}
		n, err := row.RowsAffected()
		if err != nil || n != 1 {
			return false, err
		}
	}
	return true, tx.Commit()
}
