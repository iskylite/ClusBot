package work

import (
	"context"
	"database/sql"
	"errors"
)

const BenchmarkAdmissionModelRole = "benchmark_admission"
const BenchmarkRevisionModelRole = "benchmark_revision"
const BenchmarkExecutionModelRole = "benchmark_execution"

func migrateBenchmarkExecutionModelRole(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE model_role_bindings_next (
 role TEXT PRIMARY KEY CHECK(role IN ('benchmark_admission','benchmark_revision','benchmark_execution')),
 revision INTEGER NOT NULL, profile_id TEXT NOT NULL DEFAULT '', profile_revision INTEGER NOT NULL DEFAULT 0,
 credential_binding TEXT NOT NULL DEFAULT ''
);
INSERT INTO model_role_bindings_next SELECT role,revision,profile_id,profile_revision,credential_binding FROM model_role_bindings;
DROP TABLE model_role_bindings;
ALTER TABLE model_role_bindings_next RENAME TO model_role_bindings;`)
	return err
}

func migrateBenchmarkRevisionModelRole(tx *sql.Tx) error {
	// Preserve the original role, including its credential fingerprint and
	// revision. The new role has no row until explicitly configured.
	_, err := tx.Exec(`CREATE TABLE model_role_bindings_next (
 role TEXT PRIMARY KEY CHECK(role IN ('benchmark_admission','benchmark_revision')),
 revision INTEGER NOT NULL, profile_id TEXT NOT NULL DEFAULT '', profile_revision INTEGER NOT NULL DEFAULT 0,
 credential_binding TEXT NOT NULL DEFAULT ''
);
INSERT INTO model_role_bindings_next SELECT role,revision,profile_id,profile_revision,credential_binding FROM model_role_bindings;
DROP TABLE model_role_bindings;
ALTER TABLE model_role_bindings_next RENAME TO model_role_bindings;`)
	return err
}

func migrateModelRoleBindings(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS model_role_bindings (
 role TEXT PRIMARY KEY CHECK(role='benchmark_admission'),
 revision INTEGER NOT NULL, profile_id TEXT NOT NULL DEFAULT '', profile_revision INTEGER NOT NULL DEFAULT 0,
 credential_binding TEXT NOT NULL DEFAULT ''
)`)
	return err
}

type ModelRoleBinding struct {
	Role              string
	Revision          int
	ProfileID         string
	ProfileRevision   int
	CredentialBinding string
}

func readModelRoleBinding(ctx context.Context, tx *sql.Tx, role string) (ModelRoleBinding, error) {
	b := ModelRoleBinding{Role: role}
	err := tx.QueryRowContext(ctx, `SELECT revision,profile_id,profile_revision,credential_binding FROM model_role_bindings WHERE role=?`,
		role).Scan(&b.Revision, &b.ProfileID, &b.ProfileRevision, &b.CredentialBinding)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return b, err
}

// A deleted profile does not clear a role pin. Explicit configuration loss
// remains distinguishable from the deliberately selected default route.
func (s *Store) ReadModelRole(ctx context.Context, role string) (ModelRoleBinding, ModelConnectionProfile, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ModelRoleBinding{}, ModelConnectionProfile{}, err
	}
	defer tx.Rollback()
	binding, err := readModelRoleBinding(ctx, tx, role)
	if err != nil || binding.ProfileID == "" {
		return binding, ModelConnectionProfile{}, err
	}
	p, err := scanModelProfile(tx.QueryRowContext(ctx, "SELECT "+modelProfileCols+" FROM model_connection_profiles WHERE profile_id=?", binding.ProfileID))
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return binding, p, err
}

func (s *Store) SaveModelRole(ctx context.Context, in ModelRoleBinding) (ModelRoleBinding, error) {
	if (in.Role != BenchmarkAdmissionModelRole && in.Role != BenchmarkRevisionModelRole && in.Role != BenchmarkExecutionModelRole) || in.Revision < 0 ||
		(in.ProfileID == "" && (in.ProfileRevision != 0 || in.CredentialBinding != "")) ||
		(in.ProfileID != "" && (in.ProfileRevision < 1 || in.CredentialBinding == "")) {
		return ModelRoleBinding{}, ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ModelRoleBinding{}, err
	}
	defer tx.Rollback()
	old, err := readModelRoleBinding(ctx, tx, in.Role)
	if err != nil {
		return ModelRoleBinding{}, err
	}
	if old.Revision != in.Revision {
		return ModelRoleBinding{}, ErrModelSettingsRevision
	}
	if in.ProfileID != "" {
		var revision int
		err := tx.QueryRowContext(ctx, "SELECT revision FROM model_connection_profiles WHERE profile_id=?", in.ProfileID).Scan(&revision)
		if err != nil {
			return ModelRoleBinding{}, err
		}
		if revision != in.ProfileRevision {
			return ModelRoleBinding{}, ErrModelSettingsRevision
		}
	}
	in.Revision++
	if _, err := tx.ExecContext(ctx, `INSERT INTO model_role_bindings VALUES(?,?,?,?,?)
ON CONFLICT(role) DO UPDATE SET revision=excluded.revision,profile_id=excluded.profile_id,
profile_revision=excluded.profile_revision,credential_binding=excluded.credential_binding`,
		in.Role, in.Revision, in.ProfileID, in.ProfileRevision, in.CredentialBinding); err != nil {
		return ModelRoleBinding{}, err
	}
	if err := recordAudit(tx, AuditEvent{Event: "model_role_binding_changed", Actor: "user:session", DetailRef: in.Role,
		Summary: "模型角色路由已明确更新", OccurredAt: now()}); err != nil {
		return ModelRoleBinding{}, err
	}
	return in, tx.Commit()
}
