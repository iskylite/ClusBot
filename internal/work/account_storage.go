package work

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"

	"modernc.org/sqlite"
)

func migrateAccountBusinessStorage(tx *sql.Tx) error {
	if err := migrateAccountPasswordState(tx); err != nil {
		return err
	}
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS account_business_owner (
singleton INTEGER PRIMARY KEY CHECK(singleton=1),
principal_id TEXT NOT NULL CHECK(principal_id<>'')
)`)
	return err
}

func (s *Store) AccountExists(ctx context.Context, principal string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM accounts WHERE principal_id=?)", principal).Scan(&exists)
	return exists, err
}

// BindAccountBusinessStore binds an isolated store once. It refuses a
// database installed under a different account's directory.
func (s *Store) BindAccountBusinessStore(ctx context.Context, principal string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO account_business_owner
SELECT 1,? WHERE NOT EXISTS(SELECT 1 FROM accounts)
AND NOT EXISTS(SELECT 1 FROM conversations)
AND NOT EXISTS(SELECT 1 FROM materials)
AND NOT EXISTS(SELECT 1 FROM memories)
AND NOT EXISTS(SELECT 1 FROM general_preferences)
AND NOT EXISTS(SELECT 1 FROM main.account_rsi_skills)
AND NOT EXISTS(SELECT 1 FROM main.experience_candidates)
AND NOT EXISTS(SELECT 1 FROM main.skill_evaluation_runs)
AND NOT EXISTS(SELECT 1 FROM main.skill_proposals)
ON CONFLICT(singleton) DO NOTHING`, principal); err != nil {
		return err
	}
	var owner string
	if err := tx.QueryRowContext(ctx, "SELECT principal_id FROM account_business_owner WHERE singleton=1").Scan(&owner); errors.Is(err, sql.ErrNoRows) {
		return errors.New("有数据的旧库需要先明确归属并完成迁移")
	} else if err != nil {
		return err
	}
	if owner != principal {
		return errors.New("个人数据库归属与所属目录不符")
	}
	return tx.Commit()
}

func ownsPersonalBusiness(q recordsQuerier, principal string) (bool, error) {
	var allowed bool
	err := q.QueryRow("SELECT EXISTS(SELECT 1 FROM account_business_owner WHERE singleton=1 AND principal_id=?)", principal).Scan(&allowed)
	return allowed, err
}

// accountCatalogConnector exposes the instance's read-only tool and Skill catalogs under the
// names already used by permission checks. The private database owns all writes
// made by the task runtime, including each tool invocation and its evidence.
type accountCatalogConnector struct {
	driver.Connector
	catalogURI string
}

func (c accountCatalogConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	exec := conn.(driver.ExecerContext)
	if _, err = exec.ExecContext(ctx, "ATTACH DATABASE ? AS instance_catalog", []driver.NamedValue{{Ordinal: 1, Value: c.catalogURI}}); err == nil {
		_, err = exec.ExecContext(ctx, `CREATE TEMP VIEW connections AS SELECT * FROM instance_catalog.connections;
CREATE TEMP VIEW connection_tools AS SELECT * FROM instance_catalog.connection_tools;
CREATE TEMP VIEW personal_skill_versions AS
 SELECT v.* FROM main.personal_skill_versions v JOIN main.account_rsi_skills k ON k.skill_id=v.skill_id
 UNION ALL SELECT v.* FROM instance_catalog.personal_skill_versions v
 WHERE NOT EXISTS(SELECT 1 FROM instance_catalog.skill_proposals p WHERE p.skill_id=v.skill_id AND p.version=v.version)
 AND NOT EXISTS(SELECT 1 FROM instance_catalog.account_rsi_skills k WHERE k.skill_id=v.skill_id);
CREATE TEMP VIEW personal_skills AS
 SELECT s.* FROM main.personal_skills s JOIN main.account_rsi_skills k ON k.skill_id=s.id
 UNION ALL SELECT s.id,s.scope_label,s.revision,
 CASE WHEN EXISTS(SELECT 1 FROM personal_skill_versions v WHERE v.skill_id=s.id AND v.version=s.active_version)
 THEN s.active_version END AS active_version FROM instance_catalog.personal_skills s
 WHERE NOT EXISTS(SELECT 1 FROM instance_catalog.account_rsi_skills k WHERE k.skill_id=s.id);
CREATE TEMP VIEW skill_document_sources AS SELECT d.* FROM instance_catalog.skill_document_sources d
 WHERE EXISTS(SELECT 1 FROM personal_skill_versions v WHERE v.skill_id=d.skill_id AND v.version=d.version);`, nil)
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// ConfigureAccountModelConcurrency is called once before opening any personal
// stores. Classifiers and workers thereafter share the instance's model limit.
func (s *Store) ConfigureAccountModelConcurrency(concurrency int) {
	s.accountModelSlots = make(chan struct{}, max(1, concurrency))
}

func OpenAccountBusinessStore(path string, instance *Store) (*Store, error) {
	if instance.storageLocation.path == "" {
		return nil, errors.New("多人服务器需要持久的实例数据库")
	}
	base, err := sqlite.NewConnector(path)
	if err != nil {
		return nil, err
	}
	// Migrate the on-disk private schema before attaching catalog views.
	// SQLite validates all existing views during ALTER TABLE, so a fresh or
	// pre-RSI database otherwise fails while building the source tables.
	localWriter, err := configureWriter(sql.OpenDB(base))
	if err != nil {
		return nil, err
	}
	if err := localWriter.db.Close(); err != nil {
		return nil, err
	}
	connector := accountCatalogConnector{base, sqliteFileURI(instance.storageLocation.path, "mode=ro")}
	writer, err := configureWriter(sql.OpenDB(connector))
	if err != nil {
		return nil, err
	}
	s := storeForWriter(path, writer)
	s.connectionKey = instance.connectionKey
	s.connectionGrantMu = instance.connectionGrantMu
	s.accountModelSlots = instance.accountModelSlots
	return s, nil
}

// SQLite foreign keys cannot reference an attached database. The local row
// carries only the stable ID for FK integrity. All grant checks use the live
// catalog view above, even when replaying previously persisted reads.
func rememberConnectionReference(ctx context.Context, tx *sql.Tx, connection string) error {
	if connection == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO main.connections(id,name,config,enabled)
SELECT c.id,c.id,'{}',0 FROM connections c WHERE c.id=?
AND EXISTS(SELECT 1 FROM account_business_owner)
ON CONFLICT(id) DO NOTHING`, connection)
	return err
}

// Remember only the FK identity of shared Skills locally. The catalog excludes
// these empty local placeholders; real account RSI rows are separately marked
// and stored locally, never visible through another account's instance view.
func rememberSkillReference(tx *sql.Tx, skill string, version int) error {
	if _, err := tx.Exec(`INSERT INTO main.personal_skills(id,scope_label,revision)
SELECT id,id,0 FROM personal_skills WHERE id=? AND EXISTS(SELECT 1 FROM account_business_owner)
ON CONFLICT(id) DO NOTHING`, skill); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO main.personal_skill_versions(skill_id,version,base_revision,instructions,sha256)
SELECT skill_id,version,0,'','' FROM personal_skill_versions WHERE skill_id=? AND version=?
AND EXISTS(SELECT 1 FROM account_business_owner) ON CONFLICT(skill_id,version) DO NOTHING`, skill, version)
	return err
}
